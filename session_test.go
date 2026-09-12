package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	managerrpc "github.com/telepresenceio/telepresence/rpc/v2/manager"
)

// fakeManager is a traffic-manager that answers only what a session needs, and
// records the namespace each session arrived in.
//
// That record is the whole point of it. A client session is bound to the
// namespace it arrives in, and the manager answers WatchAgentPods for that
// namespace ALONE - so "which namespaces did this process arrive in" is
// precisely "which namespaces can it ever hold a tunnel in", and it is a fact
// about the calls made rather than about anything this service reports of
// itself.
type fakeManager struct {
	managerrpc.UnimplementedManagerServer

	mu       sync.Mutex
	arrivals []string
	departed []string
}

func (m *fakeManager) Version(context.Context, *emptypb.Empty) (*managerrpc.VersionInfo2, error) {
	return &managerrpc.VersionInfo2{Name: "Fake Traffic Manager", Version: "v2.31.1"}, nil
}

func (m *fakeManager) ArriveAsClient(_ context.Context, ci *managerrpc.ClientInfo) (*managerrpc.SessionInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.arrivals = append(m.arrivals, ci.GetNamespace())
	return &managerrpc.SessionInfo{SessionId: "session-for-" + ci.GetNamespace()}, nil
}

func (m *fakeManager) GetSessionCredential(context.Context, *managerrpc.SessionInfo) (*managerrpc.SessionCredential, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented by this manager")
}

func (m *fakeManager) Remain(context.Context, *managerrpc.RemainRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (m *fakeManager) Depart(_ context.Context, si *managerrpc.SessionInfo) (*emptypb.Empty, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.departed = append(m.departed, si.GetSessionId())
	return &emptypb.Empty{}, nil
}

// The two watches hold their streams open the way the real ones do; a watch
// that returned would end the session and send the loop round again.
func (m *fakeManager) WatchAgentPods(_ *managerrpc.SessionInfo, stream grpc.ServerStreamingServer[managerrpc.AgentPodInfoSnapshot]) error {
	<-stream.Context().Done()
	return nil
}

func (m *fakeManager) WatchIntercepts(_ *managerrpc.SessionInfo, stream grpc.ServerStreamingServer[managerrpc.InterceptInfoSnapshot]) error {
	<-stream.Context().Done()
	return nil
}

func (m *fakeManager) seen() (arrivals, departed []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	arrivals = append([]string(nil), m.arrivals...)
	departed = append([]string(nil), m.departed...)
	sort.Strings(arrivals)
	sort.Strings(departed)
	return arrivals, departed
}

// serverAgainstFakeManager wires a Server to a fake manager on loopback.
func serverAgainstFakeManager(t *testing.T, namespaces ...string) (*Server, *fakeManager) {
	t.Helper()

	m := &fakeManager{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	managerrpc.RegisterManagerServer(gs, m)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	// The bearer credential re-reads the ServiceAccount token from disk on
	// every call, so there has to be one.
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("fake-token"), 0o600); err != nil {
		t.Fatalf("writing token: %v", err)
	}

	cfg := cfgFor(namespaces...)
	cfg.managerAddr = lis.Addr().String()
	cfg.clientName = "agentic-preview"
	cfg.tokenPath = tokenPath
	cfg.statePath = filepath.Join(dir, "session")
	cfg.reconnectBackoff = 50 * time.Millisecond
	cfg.remainInterval = time.Hour
	cfg.agentReconcile = time.Hour

	s := NewServer(cfg)
	return s, m
}

// TestASessionArrivesInEveryAllowedNamespace is the root cause of the held
// traffic, at the level it actually happened.
//
// Before the fix this process arrived ONCE, in allowedNamespaces[0], and that
// single session was used for every namespace. Everything downstream then
// worked exactly as designed and was still wrong: intercepts in the other
// namespaces were created and went ACTIVE, the manager provisioned their
// node-agent Jobs, and the agents connected - but the manager reports agent pods
// to a session for the session's own namespace alone, so no snapshot naming
// those pods ever reached this process, no tunnel was ever opened, and every
// request the intercept diverted was HELD waiting for a client that was not
// listening. Measured on a real cluster: swapping the order of the list moved
// which namespace hung.
//
// So the assertion is on the arrivals themselves - one per allowed namespace -
// because that is the single fact the whole failure turned on.
func TestASessionArrivesInEveryAllowedNamespace(t *testing.T) {
	s, m := serverAgainstFakeManager(t, "shop", "identity", "payments")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	eventually(t, "a session in every allowed namespace", func() bool {
		live, missing := s.connectedNamespaces()
		return len(live) == 3 && len(missing) == 0
	})

	arrivals, _ := m.seen()
	want := []string{"identity", "payments", "shop"}
	if fmt.Sprint(arrivals) != fmt.Sprint(want) {
		t.Fatalf("arrived in %v, want one session in each of %v", arrivals, want)
	}

	// Each namespace holds its OWN session, not a share of one.
	ids := map[string]bool{}
	for _, ns := range want {
		sess := s.sessionFor(ns)
		if sess == nil {
			t.Fatalf("no session for %s", ns)
		}
		if sess.ns != ns {
			t.Fatalf("session for %s is bound to %s", ns, sess.ns)
		}
		ids[sess.si.GetSessionId()] = true
	}
	if len(ids) != 3 {
		t.Fatalf("%d distinct session id(s) across 3 namespaces: %v", len(ids), ids)
	}

	// And every one of them is departed on the way out, because an intercept
	// left owned by a session nobody holds open hangs its own traffic.
	s.Shutdown(context.Background())
	_, departed := m.seen()
	if len(departed) != 3 {
		t.Fatalf("departed %v, want all three sessions", departed)
	}
}

// TestRecordedSessionsReadsBothStateFileFormats covers the one upgrade that
// matters: the pod replacement where the file was written by a version that
// held a single session. Failing to read it there leaves that session
// undeparted, and an undeparted session's intercepts hang the header they
// answer rather than falling back.
func TestRecordedSessionsReadsBothStateFileFormats(t *testing.T) {
	legacy := recordedSessions("d9d8e4f2-0000-4000-8000-000000000001\n")
	if len(legacy) != 1 || legacy[0].id != "d9d8e4f2-0000-4000-8000-000000000001" {
		t.Fatalf("legacy single-id file parsed as %+v", legacy)
	}

	current := recordedSessions("shop sid-1\nidentity sid-2\n\n")
	if len(current) != 2 {
		t.Fatalf("parsed %d record(s), want 2: %+v", len(current), current)
	}
	if current[0].ns != "shop" || current[0].id != "sid-1" {
		t.Fatalf("first record is %+v", current[0])
	}
	if current[1].ns != "identity" || current[1].id != "sid-2" {
		t.Fatalf("second record is %+v", current[1])
	}
}
