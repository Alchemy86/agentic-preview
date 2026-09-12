package main

import (
	"context"
	"net"
	"net/netip"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	agentrpc "github.com/telepresenceio/telepresence/rpc/v2/agent"
	managerrpc "github.com/telepresenceio/telepresence/rpc/v2/manager"
)

// fakeAgent is one traffic-agent, listening on loopback the way a real agent
// listens on its pod IP and its own randomised API port.
//
// It answers the two calls the pool makes - Version to prove the agent is
// there, WatchDial to hold the tunnel - and records both, plus every Tunnel a
// dial request of its own provoked. That last one is what makes "has a tunnel"
// mean something here: a dial request is only ever answered by the dial loop
// belonging to the agent that sent it, so an agent whose Tunnel was called has
// a live loop of its OWN, not a share of somebody else's.
type fakeAgent struct {
	agentrpc.UnimplementedAgentServer

	podName string
	addr    netip.AddrPort
	srv     *grpc.Server

	mu      sync.Mutex
	watches int
	tunnels int
}

func newFakeAgent(t *testing.T, podName string) *fakeAgent {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	a := &fakeAgent{
		podName: podName,
		addr:    netip.MustParseAddrPort(lis.Addr().String()),
		srv:     grpc.NewServer(),
	}
	agentrpc.RegisterAgentServer(a.srv, a)
	go func() { _ = a.srv.Serve(lis) }()
	t.Cleanup(a.srv.Stop)
	return a
}

func (a *fakeAgent) Version(context.Context, *emptypb.Empty) (*managerrpc.VersionInfo2, error) {
	return &managerrpc.VersionInfo2{Version: "v2.31.1"}, nil
}

func (a *fakeAgent) WatchDial(_ *managerrpc.SessionInfo, stream grpc.ServerStreamingServer[managerrpc.DialRequest]) error {
	a.mu.Lock()
	a.watches++
	a.mu.Unlock()

	// One dial request, then hold the stream open exactly as a real agent does
	// between intercepted connections.
	if err := stream.Send(&managerrpc.DialRequest{ConnId: []byte(a.podName)}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

func (a *fakeAgent) Tunnel(stream grpc.BidiStreamingServer[managerrpc.TunnelMessage, managerrpc.TunnelMessage]) error {
	a.mu.Lock()
	a.tunnels++
	a.mu.Unlock()
	<-stream.Context().Done()
	return nil
}

func (a *fakeAgent) counts() (watches, tunnels int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.watches, a.tunnels
}

// info is what the manager reports for this agent over WatchAgentPods.
func (a *fakeAgent) info(workload, namespace string) *managerrpc.AgentPodInfo {
	return &managerrpc.AgentPodInfo{
		WorkloadName: workload,
		PodName:      a.podName,
		Namespace:    namespace,
		PodIp:        a.addr.Addr().AsSlice(),
		ApiPort:      int32(a.addr.Port()),
		NodeAgent:    true,
		Intercepted:  true,
	}
}

// poolFor is a Server with a session but no cluster: enough for the pool, which
// only ever talks to agents.
func poolFor(t *testing.T, p *Preview) *Server {
	t.Helper()
	s := &Server{cfg: cfgFor(p.Namespace), reg: newRegistry(), sched: newScheduleStates()}
	s.cfg.agentReconcile = time.Second
	s.session = &managerrpc.SessionInfo{SessionId: "test-session"}
	s.agents = newAgentPool(s)
	if err := s.reg.add(p); err != nil {
		t.Fatalf("registry add: %v", err)
	}
	return s
}

func scheduledPreview(workload, namespace string) *Preview {
	return &Preview{
		WorkID:    "offhours",
		Workload:  workload,
		Namespace: namespace,
		Name:      workload + "-offhours",
		Global:    true,
		Schedule:  "offhours",
		PortID:    "80",
	}
}

// snapshot feeds the pool what the manager would have reported, without a
// manager.
func snapshot(p *agentPool, infos ...*managerrpc.AgentPodInfo) {
	p.mu.Lock()
	p.latest = map[string]*managerrpc.AgentPodInfo{}
	for _, ai := range infos {
		p.latest[podKeyOf(ai)] = ai
	}
	p.mu.Unlock()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestEveryReplicaGetsItsOwnTunnel is the executable form of the multi-replica
// bug, and it is written against THREE replicas rather than two on purpose: the
// failure was one tunnel replacing another, which two replicas cannot tell apart
// from a fix that special-cases a pair.
//
// Measured before the fix, on a real two-replica workload: the manager creates
// one node-agent Job per POD, the pool keyed by "<workload>.<namespace>" saw the
// second agent as the first one "changed" and rebuilt over it, and 17 of 40
// requests were served while 23 HUNG - with /schedules reporting open, up and no
// problem throughout. The assertions below are the three things that has to
// mean: a tunnel per agent pod, all of them live at once, and the count the
// manager reported carried alongside so a partial failure cannot be silent.
func TestEveryReplicaGetsItsOwnTunnel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := scheduledPreview("checkout-api", "shop")
	s := poolFor(t, p)

	agents := []*fakeAgent{
		newFakeAgent(t, "tel-node-agent-checkout-api-aaa"),
		newFakeAgent(t, "tel-node-agent-checkout-api-bbb"),
		newFakeAgent(t, "tel-node-agent-checkout-api-ccc"),
	}
	var infos []*managerrpc.AgentPodInfo
	for _, a := range agents {
		infos = append(infos, a.info("checkout-api", "shop"))
	}

	snapshot(s.agents, infos...)
	s.agents.reconcile(ctx)

	live := s.agents.status()
	if got := len(live["checkout-api.shop"]); got != 3 {
		t.Fatalf("tunnels for checkout-api.shop = %d, want 3 (one per agent pod): %v", got, live)
	}

	// Every agent holds a dial loop of its own, and every agent's own dial
	// request was answered on it.
	for _, a := range agents {
		a := a
		eventually(t, "a dial loop on "+a.podName, func() bool {
			w, tn := a.counts()
			return w == 1 && tn == 1
		})
	}

	// The API reports all three, and says how many the manager reported, so a
	// tunnel short of the replica count is visible rather than silent.
	want := []string{
		"tel-node-agent-checkout-api-aaa",
		"tel-node-agent-checkout-api-bbb",
		"tel-node-agent-checkout-api-ccc",
	}
	got := append([]string(nil), live["checkout-api.shop"]...)
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("status pods = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("status pods = %v, want %v", got, want)
		}
	}
	reg, ok := s.reg.get(p.key())
	if !ok {
		t.Fatal("preview vanished from the registry")
	}
	if len(reg.AgentPods) != 3 || reg.AgentPodsReported != 3 {
		t.Fatalf("preview reports %d tunnel(s) of %d agent pod(s), want 3 of 3",
			len(reg.AgentPods), reg.AgentPodsReported)
	}
}

// TestScalingDownDropsOnlyTheDepartedReplica is the other half of the same
// property. Reconcile has to act on the agent pod that went and leave the rest
// of the workload's tunnels ESTABLISHED - not rebuild them, which was the old
// behaviour every time the reported agent differed from the one held.
func TestScalingDownDropsOnlyTheDepartedReplica(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := scheduledPreview("checkout-api", "shop")
	s := poolFor(t, p)

	a := newFakeAgent(t, "tel-node-agent-checkout-api-aaa")
	b := newFakeAgent(t, "tel-node-agent-checkout-api-bbb")
	c := newFakeAgent(t, "tel-node-agent-checkout-api-ccc")

	snapshot(s.agents, a.info("checkout-api", "shop"), b.info("checkout-api", "shop"), c.info("checkout-api", "shop"))
	s.agents.reconcile(ctx)

	s.agents.mu.Lock()
	kept := map[string]*agentConn{}
	for k, ac := range s.agents.conns {
		kept[k] = ac
	}
	s.agents.mu.Unlock()
	if len(kept) != 3 {
		t.Fatalf("established %d tunnel(s), want 3", len(kept))
	}

	// The third replica goes; the manager stops reporting its agent.
	snapshot(s.agents, a.info("checkout-api", "shop"), b.info("checkout-api", "shop"))
	s.agents.reconcile(ctx)

	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	if len(s.agents.conns) != 2 {
		t.Fatalf("after scaling down: %d tunnel(s), want 2", len(s.agents.conns))
	}
	for _, podName := range []string{a.podName, b.podName} {
		key := podName + ".shop"
		if s.agents.conns[key] != kept[key] {
			t.Fatalf("tunnel for %s was rebuilt; only the departed replica's should have been touched", key)
		}
	}
	if _, still := s.agents.conns[c.podName+".shop"]; still {
		t.Fatalf("tunnel for the departed replica %s is still held", c.podName)
	}
}

// TestATunnelShortOfTheReplicaCountIsReported is the silence the original bug
// hid behind: the intercept is ACTIVE on every replica whether or not this
// service holds a tunnel to each, so nothing downstream can tell a whole
// workload from a fraction of one unless the reported count travels with the
// live list.
func TestATunnelShortOfTheReplicaCountIsReported(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := scheduledPreview("checkout-api", "shop")
	s := poolFor(t, p)

	a := newFakeAgent(t, "tel-node-agent-checkout-api-aaa")
	// The second agent is reported but unreachable - nothing is listening on
	// the port it names, so its tunnel cannot be established.
	dead := &managerrpc.AgentPodInfo{
		WorkloadName: "checkout-api",
		PodName:      "tel-node-agent-checkout-api-dead",
		Namespace:    "shop",
		PodIp:        netip.MustParseAddr("127.0.0.1").AsSlice(),
		ApiPort:      1, // nothing listens here
		NodeAgent:    true,
	}

	snapshot(s.agents, a.info("checkout-api", "shop"), dead)
	s.agents.reconcile(ctx)

	reg, ok := s.reg.get(p.key())
	if !ok {
		t.Fatal("preview vanished from the registry")
	}
	if len(reg.AgentPods) != 1 {
		t.Fatalf("established %d tunnel(s), want 1 (the reachable agent): %v", len(reg.AgentPods), reg.AgentPods)
	}
	if reg.AgentPodsReported != 2 {
		t.Fatalf("reported agent pods = %d, want 2 - a partial failure is invisible without it", reg.AgentPodsReported)
	}
}
