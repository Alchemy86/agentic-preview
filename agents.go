package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
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

// agentConn is one live tunnel to one agent pod.
//
// There is exactly ONE of these per AGENT POD per session, and it serves EVERY
// preview of that workload regardless of work id. That is not a simplification:
// the traffic-agent puts the destination into each dial request's connection ID
// (built from the matching intercept's target_host/target_port), so one dial
// loop already forwards each request to whichever preview matched. Opening a
// second WatchDial for the same agent and session would fight the first for the
// same slot.
//
// Per agent POD, not per workload. A workload with two replicas has two agents
// - two node-agent Jobs, or two injected sidecars - and each reports itself
// separately over WatchAgentPods and holds its OWN dial stream. A pool keyed by
// workload alone keeps one of them and silently drops the rest: the intercept
// on the un-tunnelled pod stays ACTIVE, so its share of the traffic is HELD
// rather than answered or failed over. Telepresence's own client pool
// (pkg/client/agentpf/clients.go) keys by "<podName>.<namespace>" for the same
// reason, and that is the key used here.
type agentConn struct {
	agentKey string // "<workload>.<namespace>" - what a preview needs a tunnel for
	podKey   string // "<podName>.<namespace>" - what holds one
	podName  string
	ns       string // the namespace, and so which manager session owns this tunnel
	podIP    netip.Addr
	apiPort  int32

	conn   *grpc.ClientConn
	cancel context.CancelFunc
}

// identity is what makes a tunnel stale. When the target workload rolls, the
// manager reaps the old agent and provisions a new one for the new target pod -
// a different pod, a different IP, and a freshly randomised API port - so any of
// the three changing means the tunnel must be rebuilt. The pod name is part of
// the pool key too, so in practice only the IP or the port can change under a
// name; the name stays in here because the identity is the whole triple.
func (a *agentConn) identity() string {
	return a.podName + "|" + a.podIP.String() + "|" + strconv.Itoa(int(a.apiPort))
}

func agentIdentity(ai *managerrpc.AgentPodInfo) string {
	ip, _ := netip.AddrFromSlice(ai.GetPodIp())
	return ai.GetPodName() + "|" + ip.String() + "|" + strconv.Itoa(int(ai.GetApiPort()))
}

// agentKeyOf is the workload a preview registers against.
func agentKeyOf(ai *managerrpc.AgentPodInfo) string {
	return ai.GetWorkloadName() + "." + ai.GetNamespace()
}

// podKeyOf is the pool key: one entry per agent pod.
func podKeyOf(ai *managerrpc.AgentPodInfo) string {
	return ai.GetPodName() + "." + ai.GetNamespace()
}

// agentPool keeps one tunnel per agent pod that a registered preview needs,
// and rebuilds them as the agent pod set changes.
//
// One pool, fed by EVERY namespace's session. A session is bound to one
// namespace and so is the WatchAgentPods stream it carries, but the pod keys
// those streams report already carry the namespace, and a preview asks the pool
// for a workload rather than for a session. Keeping one pool is therefore not a
// compromise: it is the one place that knows, across all namespaces at once,
// which workloads are short of a tunnel.
type agentPool struct {
	srv *Server

	mu     sync.Mutex
	conns  map[string]*agentConn               // by pod key
	latest map[string]*managerrpc.AgentPodInfo // last snapshot, by pod key
	// establishing is the pod keys a reconcile pass has claimed but not yet
	// finished dialling. It exists because an agent has ONE dial slot: a
	// second WatchDial to the same agent and session displaces the first, so
	// two passes dialling the same pod leave the agent holding a watcher that
	// the losing pass then cancels - and the pool still reporting a tunnel.
	// The pod is then ACTIVE with nothing listening, which HOLDS its traffic.
	//
	// One watcher could not race itself; a watcher per namespace can, because
	// they reconcile the one pool concurrently. Measured after that change: a
	// two-replica workload served one replica and hung the other, with both
	// tunnels reported up, which is indistinguishable from the bug the per-pod
	// keying fixed and is not the same bug at all.
	establishing map[string]bool
	kick         chan struct{}
}

func newAgentPool(srv *Server) *agentPool {
	return &agentPool{
		srv:          srv,
		conns:        map[string]*agentConn{},
		latest:       map[string]*managerrpc.AgentPodInfo{},
		establishing: map[string]bool{},
		kick:         make(chan struct{}, 1),
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
// itself. It does the same when the workload SCALES: a new replica gets its own
// Job, a removed one has its Job reaped, and both arrive here as a snapshot with
// a different set of pod keys. The intercept is manager state keyed by name and
// session, so it survives all of that untouched. All agentic-preview has to do -
// and all an earlier one-shot prototype failed to do - is notice the agent pod
// set changed and hold a tunnel to every pod in it.
//
// A local ticker runs alongside the stream because a dial loop can end on its
// own (the agent pod died) without a new snapshot arriving to prompt a rebuild.
//
// One of these runs per namespace, on that namespace's session. The manager
// answers WatchAgentPods for the session's own namespace and no other, so each
// stream is authoritative about its namespace and says nothing at all about the
// rest - which is why a snapshot replaces only its own namespace's entries
// below, and not the whole map.
func (p *agentPool) watch(ctx context.Context, mc managerrpc.ManagerClient, si *managerrpc.SessionInfo, ns string) error {
	st, err := mc.WatchAgentPods(ctx, si)
	if err != nil {
		return fmt.Errorf("WatchAgentPods in %s: %w", ns, err)
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
			return fmt.Errorf("WatchAgentPods in %s recv: %w", ns, err)
		case agents := <-snaps:
			p.replaceNamespace(ns, agents)
			p.reconcile(ctx)
		case <-p.kick:
			p.reconcile(ctx)
		case <-t.C:
			p.reconcile(ctx)
		}
	}
}

// replaceNamespace swaps in one namespace's agent pods, leaving every other
// namespace's entries alone.
//
// This is the reason the snapshot is not simply assigned. Each namespace's
// stream reports only its own namespace, so a wholesale replacement would make
// every snapshot from one namespace erase what the others had reported, and the
// pool would hold tunnels for whichever namespace reported most recently. That
// is the same "one at a time" failure the per-pod keying fixed one level down.
func (p *agentPool) replaceNamespace(ns string, agents []*managerrpc.AgentPodInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for podKey, ai := range p.latest {
		if ai.GetNamespace() == ns {
			delete(p.latest, podKey)
		}
	}
	for _, ai := range agents {
		p.latest[podKeyOf(ai)] = ai
	}
}

// reconcile brings the set of tunnels in line with the set of workloads the
// registry needs and the agent pods the manager last reported. Every reported
// pod of a wanted workload gets its own tunnel; none of them replaces another.
func (p *agentPool) reconcile(ctx context.Context) {
	wanted := p.srv.reg.workloads()

	p.mu.Lock()
	// Snapshot what we need while holding the lock, then act outside it:
	// establishing a tunnel dials and blocks.
	type todo struct {
		podKey string
		ai     *managerrpc.AgentPodInfo
	}
	var establish []todo
	var drop []*agentConn

	for podKey, ac := range p.conns {
		ai, reported := p.latest[podKey]
		switch {
		case !wanted[ac.agentKey]:
			// No preview needs this workload any more.
			drop = append(drop, ac)
			delete(p.conns, podKey)
		case !reported:
			// The manager no longer reports this agent pod - the replica went,
			// or the workload rolled and the pod was replaced by a new name.
			drop = append(drop, ac)
			delete(p.conns, podKey)
		case agentIdentity(ai) != ac.identity():
			// Same pod name, different IP or API port. Rebuild.
			logf("agent pod %s changed (%s -> %s); rebuilding tunnel", podKey, ac.identity(), agentIdentity(ai))
			drop = append(drop, ac)
			delete(p.conns, podKey)
			establish = append(establish, todo{podKey, ai})
		}
	}
	for podKey, ai := range p.latest {
		if !wanted[agentKeyOf(ai)] {
			continue
		}
		if _, live := p.conns[podKey]; live {
			continue
		}
		if p.establishing[podKey] {
			// Another pass is already dialling this agent. Dialling it too
			// would put a second WatchDial in its single dial slot.
			continue
		}
		establish = append(establish, todo{podKey, ai})
	}
	// Claimed here, under the same lock that decided them, so a concurrent
	// pass sees the claim rather than the not-yet-established tunnel.
	for _, td := range establish {
		p.establishing[td.podKey] = true
	}
	p.mu.Unlock()

	for _, ac := range drop {
		p.close(ac)
	}
	for _, td := range establish {
		if ctx.Err() != nil {
			return
		}
		if err := p.establish(ctx, td.ai); err != nil {
			logf("establishing tunnel for %s: %v", td.podKey, err)
		}
	}

	p.publish()
}

// publish re-asserts, for every workload a preview wants, which agent pods carry
// a tunnel and how many the manager says there are.
//
// It runs on EVERY pass, not only when something was established: a preview
// added after its workload's tunnels already existed would otherwise never have
// been told, and would report no agent pods in the list API. The reported count
// goes with the live list because the two differing is the whole multi-replica
// failure - a workload with three agents and two tunnels is serving two thirds
// of its traffic and holding the rest, and nothing else in the service can see
// that.
func (p *agentPool) publish() {
	p.mu.Lock()
	live := map[string][]string{}
	for _, ac := range p.conns {
		live[ac.agentKey] = append(live[ac.agentKey], ac.podName)
	}
	reported := map[string]int{}
	for _, ai := range p.latest {
		reported[agentKeyOf(ai)]++
	}
	p.mu.Unlock()

	for _, key := range p.srv.reg.workloadList() {
		pods := live[key]
		sort.Strings(pods)
		p.srv.reg.setAgentPods(key, pods, reported[key])
	}
}

// establish dials one agent pod and runs its dial loop.
//
// The agent pod is dialled DIRECTLY at its own pod IP and its own randomised
// API port, both read from the snapshot. Neither can be assumed: the manager
// reports a node-agent under the WORKLOAD's namespace while the Job itself runs
// in the manager's namespace, and the API port is fresh per Job.
func (p *agentPool) establish(ctx context.Context, ai *managerrpc.AgentPodInfo) error {
	agentKey, podKey, ns := agentKeyOf(ai), podKeyOf(ai), ai.GetNamespace()

	// The claim reconcile took on this pod key is released however this
	// returns - the entry in conns is what a later pass reads, and a claim
	// left behind would stop the tunnel ever being rebuilt.
	defer func() {
		p.mu.Lock()
		delete(p.establishing, podKey)
		p.mu.Unlock()
	}()

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

	vctx, vcancel := context.WithTimeout(p.srv.agentMetadata(actx, ns), 20*time.Second)
	_, err = ac.Version(vctx, &emptypb.Empty{})
	vcancel()
	if err != nil {
		cancel()
		conn.Close()
		return fmt.Errorf("agent Version at %s: %w", addr, err)
	}

	// The session of the agent's OWN namespace. WatchDial registers this
	// client session as the one the agent sends dial requests to, so a tunnel
	// opened under another namespace's session would be registered against a
	// session that never intercepted anything here.
	sess := p.srv.sessionFor(ns)
	if sess == nil {
		cancel()
		conn.Close()
		return fmt.Errorf("no manager session for namespace %s", ns)
	}
	si := sess.si

	dialStream, err := ac.WatchDial(p.srv.agentMetadata(actx, ns), si)
	if err != nil {
		cancel()
		conn.Close()
		return fmt.Errorf("agent WatchDial at %s: %w", addr, err)
	}

	entry := &agentConn{
		agentKey: agentKey,
		podKey:   podKey,
		podName:  ai.GetPodName(),
		ns:       ns,
		podIP:    podIP,
		apiPort:  ai.GetApiPort(),
		conn:     conn,
		cancel:   cancel,
	}

	p.mu.Lock()
	// Should be unreachable: reconcile claims the pod key in `establishing`
	// before dialling, so no second pass gets this far for the same agent.
	// Kept as the last resort for any future caller that does not claim -
	// note that reaching it has already cost the winner its dial slot, so it
	// is a bug to rely on rather than a branch to take.
	if _, exists := p.conns[podKey]; exists {
		p.mu.Unlock()
		cancel()
		conn.Close()
		return nil
	}
	p.conns[podKey] = entry
	p.mu.Unlock()

	logf("tunnel up for %s via %s at %s (node-agent=%v)", agentKey, ai.GetPodName(), addr, ai.GetNodeAgent())

	// DialWaitLoop is the whole forwarder. The traffic-agent never dials
	// target_host itself: it opens a stream back to this client session for
	// each intercepted request, and the dial happens HERE, with a plain
	// net.Dial to the address carried in the connection ID. In-cluster that
	// reaches any ClusterIP, which is what lets a preview Deployment be the
	// far end of a header.
	//
	// Each agent pod runs its own loop over its own stream, and an intercepted
	// request only ever arrives on the loop belonging to the pod that received
	// it. Nothing here has to fan traffic out across the replicas: the agents
	// already did, by being where the traffic landed.
	go func() {
		err := tunnel.DialWaitLoop(actx, tunnel.AgentToClient, tunnel.AgentProvider(ac),
			dialStream, tunnel.SessionID(si.GetSessionId()), nil)
		if err != nil && actx.Err() == nil {
			logf("dial loop for %s ended: %v", podKey, err)
		}
		// Drop ourselves so the next reconcile rebuilds. Only if we are still
		// the current entry: a rebuild may already have replaced us.
		p.mu.Lock()
		if cur, ok := p.conns[podKey]; ok && cur == entry {
			delete(p.conns, podKey)
		}
		p.mu.Unlock()
		cancel()
		conn.Close()
		if actx.Err() == nil {
			p.publish()
			p.reconcileNow()
		}
	}()
	return nil
}

func (p *agentPool) close(ac *agentConn) {
	logf("tunnel down for %s (%s)", ac.agentKey, ac.podName)
	ac.cancel()
	_ = ac.conn.Close()
}

// closeNamespace drops everything belonging to one namespace's session, which
// is what a session ending means: its tunnels are dead and its agent pods are
// no longer being reported to anybody. Every other namespace is untouched -
// tearing the whole pool down when one session dropped would take working
// namespaces with it.
func (p *agentPool) closeNamespace(ns string) {
	p.mu.Lock()
	var conns []*agentConn
	for podKey, ac := range p.conns {
		if ac.ns == ns {
			conns = append(conns, ac)
			delete(p.conns, podKey)
		}
	}
	for podKey, ai := range p.latest {
		if ai.GetNamespace() == ns {
			delete(p.latest, podKey)
		}
	}
	p.mu.Unlock()
	for _, ac := range conns {
		p.close(ac)
	}
	p.publish()
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
	p.publish()
}

// status reports the live tunnels, for the API: every agent pod holding one,
// under the workload it belongs to.
func (p *agentPool) status() map[string][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string][]string{}
	for _, ac := range p.conns {
		out[ac.agentKey] = append(out[ac.agentKey], ac.podName)
	}
	for _, pods := range out {
		sort.Strings(pods)
	}
	return out
}
