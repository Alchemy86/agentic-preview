package main

import (
	"context"
	"net"
	"net/netip"
	"slices"
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

// poolFor is a Server with a session per namespace but no cluster: enough for
// the pool, which only ever talks to agents. The namespaces are the ones the
// given previews are in, each with its own session, exactly as the real process
// holds one session per allowed namespace.
func poolFor(t *testing.T, previews ...*Preview) *Server {
	t.Helper()
	var nss []string
	for _, p := range previews {
		if !slices.Contains(nss, p.Namespace) {
			nss = append(nss, p.Namespace)
		}
	}
	s := &Server{
		cfg:      cfgFor(nss...),
		reg:      newRegistry(),
		sched:    newScheduleStates(),
		sessions: map[string]*mgrSession{},
		saved:    map[string]string{},
	}
	s.cfg.agentReconcile = time.Second
	for _, ns := range nss {
		s.sessions[ns] = &mgrSession{
			ns: ns,
			si: &managerrpc.SessionInfo{SessionId: "test-session-" + ns},
		}
	}
	s.agents = newAgentPool(s)
	for _, p := range previews {
		if err := s.reg.add(p); err != nil {
			t.Fatalf("registry add: %v", err)
		}
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

// snapshot feeds the pool what the manager would have reported for ONE
// namespace, without a manager.
//
// Per namespace because that is the only shape a snapshot ever arrives in: the
// manager answers WatchAgentPods for the session's own namespace alone, so each
// stream is a complete statement about its namespace and says nothing about any
// other. A test that fed the pool every namespace at once would not be able to
// catch one namespace's stream erasing another's.
func snapshot(p *agentPool, ns string, infos ...*managerrpc.AgentPodInfo) {
	p.replaceNamespace(ns, infos)
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

	snapshot(s.agents, "shop", infos...)
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

	snapshot(s.agents, "shop", a.info("checkout-api", "shop"), b.info("checkout-api", "shop"), c.info("checkout-api", "shop"))
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
	snapshot(s.agents, "shop", a.info("checkout-api", "shop"), b.info("checkout-api", "shop"))
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

	snapshot(s.agents, "shop", a.info("checkout-api", "shop"), dead)
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

// TestEveryAllowedNamespaceIsServedAtOnce is the executable form of the
// session-per-namespace bug.
//
// Measured on a real cluster before the fix, with ALLOWED_NAMESPACES set to two
// namespaces: the first one was served and the second one's requests HUNG. Not
// failed - held. Its intercept was created, went ACTIVE and stayed ACTIVE, and
// every status this service reported said so; what never arrived was a tunnel,
// because the single client session had arrived in the first namespace and the
// manager answers WatchAgentPods for the session's own namespace alone. Flipping
// the order moved the breakage to the other namespace rather than fixing it,
// which is what proved it was the session and not RBAC, the node-agent Jobs or
// the schedule.
//
// So the property is not "a second namespace can work". It is that BOTH work
// SIMULTANEOUSLY, on their own sessions, with each namespace's snapshots
// arriving on their own stream - interleaved here, because a stream that
// replaced the whole pool instead of its own namespace's share would pass a test
// that fed them in one go.
func TestEveryAllowedNamespaceIsServedAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shop := scheduledPreview("checkout-api", "shop")
	auth := scheduledPreview("keycloak", "identity")
	s := poolFor(t, shop, auth)

	shopA := newFakeAgent(t, "tel-node-agent-checkout-api-aaa")
	shopB := newFakeAgent(t, "tel-node-agent-checkout-api-bbb")
	authA := newFakeAgent(t, "tel-node-agent-keycloak-aaa")
	authB := newFakeAgent(t, "tel-node-agent-keycloak-bbb")

	// Each namespace's stream reports its own namespace, one after the other,
	// with a reconcile in between - the interleaving a real pair of watchers
	// produces.
	snapshot(s.agents, "shop", shopA.info("checkout-api", "shop"), shopB.info("checkout-api", "shop"))
	s.agents.reconcile(ctx)
	snapshot(s.agents, "identity", authA.info("keycloak", "identity"), authB.info("keycloak", "identity"))
	s.agents.reconcile(ctx)

	// And once more from the first namespace, which is where a pool that
	// replaced everything on each snapshot would have dropped the second.
	snapshot(s.agents, "shop", shopA.info("checkout-api", "shop"), shopB.info("checkout-api", "shop"))
	s.agents.reconcile(ctx)

	live := s.agents.status()
	if got := len(live["checkout-api.shop"]); got != 2 {
		t.Fatalf("tunnels for checkout-api.shop = %d, want 2: %v", got, live)
	}
	if got := len(live["keycloak.identity"]); got != 2 {
		t.Fatalf("tunnels for keycloak.identity = %d, want 2: %v", got, live)
	}

	// A live dial loop on every agent in BOTH namespaces. This is the
	// assertion the bug failed: the second namespace's agents were never
	// dialled at all, so nothing answered the requests their intercept held.
	for _, a := range []*fakeAgent{shopA, shopB, authA, authB} {
		a := a
		eventually(t, "a dial loop on "+a.podName, func() bool {
			w, tn := a.counts()
			return w == 1 && tn == 1
		})
	}

	// Each tunnel was opened on the session belonging to its OWN namespace.
	// Telling the agent about another namespace's session would register the
	// dial watcher against a session that never intercepted anything there.
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	for podKey, ac := range s.agents.conns {
		if ac.ns != s.sessions[ac.ns].ns {
			t.Fatalf("%s is held under namespace %q", podKey, ac.ns)
		}
	}
	for _, want := range []struct{ podKey, ns string }{
		{"tel-node-agent-checkout-api-aaa.shop", "shop"},
		{"tel-node-agent-keycloak-aaa.identity", "identity"},
	} {
		ac, ok := s.agents.conns[want.podKey]
		if !ok {
			t.Fatalf("no tunnel for %s", want.podKey)
		}
		if ac.ns != want.ns {
			t.Fatalf("tunnel %s is on namespace %q, want %q", want.podKey, ac.ns, want.ns)
		}
	}
}

// TestOneNamespaceLosingItsSessionLeavesTheOthersUp is the other half of the
// same property, and the reason the pool tears down per namespace rather than
// wholesale.
//
// A session dies on its own schedule - the manager restarts, a session expires -
// and only that namespace's supervising loop rebuilds it. If the end of one
// session dropped every tunnel in the pool, a manager hiccup in one namespace
// would hold every other namespace's traffic until each was re-established:
// which is the original bug again, just triggered by a reconnect instead of by
// the order of a list.
func TestOneNamespaceLosingItsSessionLeavesTheOthersUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shop := scheduledPreview("checkout-api", "shop")
	auth := scheduledPreview("keycloak", "identity")
	s := poolFor(t, shop, auth)

	shopA := newFakeAgent(t, "tel-node-agent-checkout-api-aaa")
	authA := newFakeAgent(t, "tel-node-agent-keycloak-aaa")

	snapshot(s.agents, "shop", shopA.info("checkout-api", "shop"))
	snapshot(s.agents, "identity", authA.info("keycloak", "identity"))
	s.agents.reconcile(ctx)

	s.agents.mu.Lock()
	kept := s.agents.conns["tel-node-agent-checkout-api-aaa.shop"]
	n := len(s.agents.conns)
	s.agents.mu.Unlock()
	if n != 2 || kept == nil {
		t.Fatalf("established %d tunnel(s), want 2 including checkout-api's", n)
	}

	// The identity session ends, exactly as runSession's deferred teardown
	// does it.
	s.mu.Lock()
	delete(s.sessions, "identity")
	s.mu.Unlock()
	s.agents.closeNamespace("identity")

	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	if _, still := s.agents.conns["tel-node-agent-keycloak-aaa.identity"]; still {
		t.Fatal("the departed namespace's tunnel is still held")
	}
	if got := s.agents.conns["tel-node-agent-checkout-api-aaa.shop"]; got != kept {
		t.Fatalf("shop's tunnel was torn down or rebuilt when identity's session ended")
	}
	if ai := s.agents.latest["tel-node-agent-keycloak-aaa.identity"]; ai != nil {
		t.Fatal("the departed namespace's agent pods are still reported")
	}
	if ai := s.agents.latest["tel-node-agent-checkout-api-aaa.shop"]; ai == nil {
		t.Fatal("shop's agent pods were forgotten with identity's session")
	}
}

// TestConcurrentReconcilesDialEachAgentOnce is the regression test for the race
// that a watcher per namespace made reachable.
//
// A traffic-agent has ONE dial slot per client session. A second WatchDial to
// the same agent displaces the first, so two reconcile passes dialling the same
// pod leave the agent holding the loser's watcher, which the loser then cancels
// on finding it lost the pool entry - and the pool goes on reporting a tunnel
// for a pod that has nobody listening. Its traffic is HELD, which is exactly the
// failure this whole change exists to remove, arrived at from the other
// direction.
//
// One watcher could not race itself. Measured on a real cluster with a watcher
// per namespace and a two-replica workload: one replica served, the other hung,
// both tunnels reported up.
//
// The assertion is on the AGENT's count of WatchDial calls, not on the pool's
// view of itself, because the pool's view was correct throughout the failure.
func TestConcurrentReconcilesDialEachAgentOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shop := scheduledPreview("checkout-api", "shop")
	auth := scheduledPreview("keycloak", "identity")
	s := poolFor(t, shop, auth)

	shopA := newFakeAgent(t, "tel-node-agent-checkout-api-aaa")
	shopB := newFakeAgent(t, "tel-node-agent-checkout-api-bbb")
	authA := newFakeAgent(t, "tel-node-agent-keycloak-aaa")
	authB := newFakeAgent(t, "tel-node-agent-keycloak-bbb")
	agents := []*fakeAgent{shopA, shopB, authA, authB}

	snapshot(s.agents, "shop", shopA.info("checkout-api", "shop"), shopB.info("checkout-api", "shop"))
	snapshot(s.agents, "identity", authA.info("keycloak", "identity"), authB.info("keycloak", "identity"))

	// Several passes at once, as two namespace watchers reconciling the one
	// pool produce: each has its own stream, its own ticker and its share of
	// the kicks a raise sends.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.agents.reconcile(ctx)
		}()
	}
	wg.Wait()

	for _, a := range agents {
		a := a
		eventually(t, "a dial loop on "+a.podName, func() bool {
			w, tn := a.counts()
			return w >= 1 && tn >= 1
		})
	}

	// Exactly one WatchDial per agent. More than one means the agent's dial
	// slot was taken twice, and the tunnel the pool believes it holds is the
	// one that lost.
	for _, a := range agents {
		if w, _ := a.counts(); w != 1 {
			t.Fatalf("%s saw %d WatchDial call(s), want exactly 1: its dial slot was claimed more than once", a.podName, w)
		}
	}
	if got := len(s.agents.status()["checkout-api.shop"]) + len(s.agents.status()["keycloak.identity"]); got != 4 {
		t.Fatalf("pool holds %d tunnel(s), want 4", got)
	}
}
