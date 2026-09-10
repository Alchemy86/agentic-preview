package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	agentrpc "github.com/telepresenceio/telepresence/rpc/v2/agent"
	managerrpc "github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/tunnel"
)

// agentConn is one live tunnel to one node-agent pod.
//
// There is exactly ONE of these per agent pod per session, and it serves EVERY
// preview of that workload regardless of work id. That is not a simplification:
// the traffic-agent puts the destination into each dial request's connection ID
// (built from the matching intercept's target_host/target_port), so one dial
// loop already forwards each request to whichever preview matched. Opening a
// second WatchDial for the same agent and session would fight the first for the
// same slot.
type agentConn struct {
	key     string // "<workload>.<namespace>"
	podName string
	podIP   netip.Addr
	apiPort int32

	conn   *grpc.ClientConn
	cancel context.CancelFunc
}

// identity is what makes a tunnel stale. When the target workload rolls, the
// manager reaps the old node-agent Job and provisions a new one for the new
// target pod - a different pod, a different IP, and a freshly randomised API
// port - so any of the three changing means the tunnel must be rebuilt.
func (a *agentConn) identity() string {
	return a.podName + "|" + a.podIP.String() + "|" + strconv.Itoa(int(a.apiPort))
}

func agentIdentity(ai *managerrpc.AgentPodInfo) string {
	ip, _ := netip.AddrFromSlice(ai.GetPodIp())
	return ai.GetPodName() + "|" + ip.String() + "|" + strconv.Itoa(int(ai.GetApiPort()))
}

// agentPool keeps one tunnel per agent pod that a registered preview needs,
// and rebuilds them as the agent pod set changes.
type agentPool struct {
	srv *Server

	mu     sync.Mutex
	conns  map[string]*agentConn
	latest map[string]*managerrpc.AgentPodInfo // last snapshot, by agent key
	kick   chan struct{}
}

func newAgentPool(srv *Server) *agentPool {
	return &agentPool{
		srv:    srv,
		conns:  map[string]*agentConn{},
		latest: map[string]*managerrpc.AgentPodInfo{},
		kick:   make(chan struct{}, 1),
	}
}

// reconcileNow asks for a reconcile pass without waiting for a snapshot or the
// ticker - used right after an intercept is raised or removed.
func (p *agentPool) reconcileNow() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// watch consumes WatchAgentPods for the life of a session and keeps the tunnels
// reconciled to it.
//
// This is the mechanism that survives a target-pod rollout. The manager runs its
// own node-agent reconciler against the workload's live pod set (1s debounce,
// 30s resync) for as long as an intercept claim exists, so when the target
// workload rolls it reaps the dead pod's Job and creates one for the new pod by
// itself. The intercept is manager state keyed by name and session, so it
// survives that untouched. All agentic-preview has to do - and all an earlier
// one-shot prototype failed to do - is notice the agent pod changed and rebuild
// the tunnel to it.
//
// A local ticker runs alongside the stream because a dial loop can end on its
// own (the agent pod died) without a new snapshot arriving to prompt a rebuild.
func (p *agentPool) watch(ctx context.Context, mc managerrpc.ManagerClient, si *managerrpc.SessionInfo) error {
	st, err := mc.WatchAgentPods(ctx, si)
	if err != nil {
		return fmt.Errorf("WatchAgentPods: %w", err)
	}

	snaps := make(chan []*managerrpc.AgentPodInfo, 8)
	errCh := make(chan error, 1)
	go func() {
		for {
			snap, err := st.Recv()
			if err != nil {
				errCh <- err
				return
			}
			select {
			case snaps <- snap.GetAgents():
			case <-ctx.Done():
				return
			}
		}
	}()

	t := time.NewTicker(p.srv.cfg.agentReconcile)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("WatchAgentPods recv: %w", err)
		case agents := <-snaps:
			p.mu.Lock()
			p.latest = map[string]*managerrpc.AgentPodInfo{}
			for _, ai := range agents {
				p.latest[ai.GetWorkloadName()+"."+ai.GetNamespace()] = ai
			}
			p.mu.Unlock()
			p.reconcile(ctx)
		case <-p.kick:
			p.reconcile(ctx)
		case <-t.C:
			p.reconcile(ctx)
		}
	}
}

// reconcile brings the set of tunnels in line with the set of workloads the
// registry needs and the agent pods the manager last reported.
func (p *agentPool) reconcile(ctx context.Context) {
	wanted := p.srv.reg.workloads()

	p.mu.Lock()
	// Snapshot what we need while holding the lock, then act outside it:
	// establishing a tunnel dials and blocks.
	type todo struct {
		key string
		ai  *managerrpc.AgentPodInfo
	}
	var establish []todo
	var drop []*agentConn

	for key, ac := range p.conns {
		ai, reported := p.latest[key]
		switch {
		case !wanted[key]:
			// No preview needs this workload any more.
			drop = append(drop, ac)
			delete(p.conns, key)
		case !reported:
			// The manager no longer reports an agent pod for it.
			drop = append(drop, ac)
			delete(p.conns, key)
		case agentIdentity(ai) != ac.identity():
			// The agent pod was replaced - a rollout. Rebuild.
			logf("agent pod for %s changed (%s -> %s); rebuilding tunnel", key, ac.identity(), agentIdentity(ai))
			drop = append(drop, ac)
			delete(p.conns, key)
			establish = append(establish, todo{key, ai})
		}
	}
	for key := range wanted {
		if _, live := p.conns[key]; live {
			continue
		}
		if ai, reported := p.latest[key]; reported {
			establish = append(establish, todo{key, ai})
		}
	}
	p.mu.Unlock()

	for _, ac := range drop {
		p.close(ac)
	}
	for _, td := range establish {
		if ctx.Err() != nil {
			return
		}
		if err := p.establish(ctx, td.key, td.ai); err != nil {
			logf("establishing tunnel for %s: %v", td.key, err)
		}
	}

	// Re-assert which node-agent carries each live tunnel. A preview added
	// after its workload's tunnel already existed would otherwise never have
	// been told, and would report an empty agentPod in the list API.
	p.mu.Lock()
	live := make(map[string]string, len(p.conns))
	for k, ac := range p.conns {
		live[k] = ac.podName
	}
	p.mu.Unlock()
	for k, pod := range live {
		p.srv.reg.setAgentPod(k, pod)
	}
}

// establish dials one node-agent pod and runs its dial loop.
//
// The agent pod is dialled DIRECTLY at its own pod IP and its own randomised
// API port, both read from the snapshot. Neither can be assumed: the manager
// reports a node-agent under the WORKLOAD's namespace while the Job itself runs
// in the manager's namespace, and the API port is fresh per Job.
func (p *agentPool) establish(ctx context.Context, key string, ai *managerrpc.AgentPodInfo) error {
	podIP, ok := netip.AddrFromSlice(ai.GetPodIp())
	if !ok {
		return fmt.Errorf("agent %s reported an unusable pod IP", ai.GetPodName())
	}
	if ai.GetApiPort() == 0 {
		return fmt.Errorf("agent %s reported no API port", ai.GetPodName())
	}
	addr := net.JoinHostPort(podIP.String(), strconv.Itoa(int(ai.GetApiPort())))

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial agent %s: %w", addr, err)
	}
	ac := agentrpc.NewAgentClient(conn)

	actx, cancel := context.WithCancel(ctx)

	vctx, vcancel := context.WithTimeout(p.srv.agentMetadata(actx), 20*time.Second)
	_, err = ac.Version(vctx, &emptypb.Empty{})
	vcancel()
	if err != nil {
		cancel()
		conn.Close()
		return fmt.Errorf("agent Version at %s: %w", addr, err)
	}

	_, si, _ := p.srv.state()
	if si == nil {
		cancel()
		conn.Close()
		return fmt.Errorf("session went away")
	}

	dialStream, err := ac.WatchDial(p.srv.agentMetadata(actx), si)
	if err != nil {
		cancel()
		conn.Close()
		return fmt.Errorf("agent WatchDial at %s: %w", addr, err)
	}

	entry := &agentConn{
		key:     key,
		podName: ai.GetPodName(),
		podIP:   podIP,
		apiPort: ai.GetApiPort(),
		conn:    conn,
		cancel:  cancel,
	}

	p.mu.Lock()
	// Another pass may have got here first.
	if _, exists := p.conns[key]; exists {
		p.mu.Unlock()
		cancel()
		conn.Close()
		return nil
	}
	p.conns[key] = entry
	p.mu.Unlock()

	p.srv.reg.setAgentPod(key, ai.GetPodName())
	logf("tunnel up for %s via %s at %s (node-agent=%v)", key, ai.GetPodName(), addr, ai.GetNodeAgent())

	// DialWaitLoop is the whole forwarder. The traffic-agent never dials
	// target_host itself: it opens a stream back to this client session for
	// each intercepted request, and the dial happens HERE, with a plain
	// net.Dial to the address carried in the connection ID. In-cluster that
	// reaches any ClusterIP, which is what lets a preview Deployment be the
	// far end of a header.
	go func() {
		err := tunnel.DialWaitLoop(actx, tunnel.AgentToClient, tunnel.AgentProvider(ac),
			dialStream, tunnel.SessionID(si.GetSessionId()), nil)
		if err != nil && actx.Err() == nil {
			logf("dial loop for %s ended: %v", key, err)
		}
		// Drop ourselves so the next reconcile rebuilds. Only if we are still
		// the current entry: a rebuild may already have replaced us.
		p.mu.Lock()
		if cur, ok := p.conns[key]; ok && cur == entry {
			delete(p.conns, key)
		}
		p.mu.Unlock()
		cancel()
		conn.Close()
		if actx.Err() == nil {
			p.srv.reg.setAgentPod(key, "")
			p.reconcileNow()
		}
	}()
	return nil
}

func (p *agentPool) close(ac *agentConn) {
	logf("tunnel down for %s (%s)", ac.key, ac.podName)
	ac.cancel()
	_ = ac.conn.Close()
	p.srv.reg.setAgentPod(ac.key, "")
}

func (p *agentPool) closeAll() {
	p.mu.Lock()
	conns := p.conns
	p.conns = map[string]*agentConn{}
	p.latest = map[string]*managerrpc.AgentPodInfo{}
	p.mu.Unlock()
	for _, ac := range conns {
		p.close(ac)
	}
}

// status reports the live tunnels, for the API.
func (p *agentPool) status() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]string{}
	for k, ac := range p.conns {
		out[k] = ac.podName
	}
	return out
}
