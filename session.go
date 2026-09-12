package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	managerrpc "github.com/telepresenceio/telepresence/rpc/v2/manager"
)

// bearer presents the ServiceAccount token to the manager as
// "authorization: bearer <token>". The manager's auth interceptor reads that
// one header and hands the token to a Kubernetes TokenReview.
//
// The token is read on every call, not cached: a projected ServiceAccount
// token is rotated in place by the kubelet, and a process that lives for days
// would otherwise start presenting an expired one.
type bearer struct{ path string }

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	tok, err := os.ReadFile(b.path)
	if err != nil {
		return nil, fmt.Errorf("reading service account token %s: %w", b.path, err)
	}
	return map[string]string{"authorization": "bearer " + strings.TrimSpace(string(tok))}, nil
}

func (bearer) RequireTransportSecurity() bool { return false }

// Server holds one manager session and the tunnels that serve every live
// preview. Exactly one Server exists per process.
type Server struct {
	cfg *config
	reg *registry

	mu      sync.RWMutex
	conn    *grpc.ClientConn
	mc      managerrpc.ManagerClient
	session *managerrpc.SessionInfo
	// sessionToken is the session-scoped credential presented to a
	// traffic-agent. Empty when the manager cannot mint one.
	sessionToken string
	// managerVersion is reported by the API for diagnosis.
	managerVersion string
	agents         *agentPool
	// kube is the Kubernetes API client used to BUILD previews. It is nil when
	// there is no in-cluster config - the routing half needs nothing from
	// Kubernetes, so that is a per-request refusal rather than a fatal one.
	// kubeErr says why, so the refusal can name the reason.
	kube    kubeAPI
	kubeErr error
	// ready is closed once a session exists, so the API can report honestly.
	connected bool
	// raiseMu serialises intercept creation: PrepareIntercept provisions the
	// node-agent Job, and two concurrent Prepares for one workload race.
	raiseMu sync.Mutex
	// sched is the schedule controller's own memory, keyed by schedule name.
	// Empty and untouched when no schedule is declared.
	sched *scheduleStates
}

func NewServer(cfg *config) *Server {
	s := &Server{cfg: cfg, reg: newRegistry(), sched: newScheduleStates()}
	s.agents = newAgentPool(s)
	if k, err := newKubeAPI(); err != nil {
		s.kubeErr = err
		logf("no Kubernetes API client: %v - previews must be deployed elsewhere and named with previewService", err)
	} else {
		s.kube = k
	}
	return s
}

// state returns the current session and manager client together, so a caller
// never mixes a session id with a client from a later incarnation.
func (s *Server) state() (managerrpc.ManagerClient, *managerrpc.SessionInfo, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mc, s.session, s.sessionToken
}

// Run is the supervising loop. Each pass builds a whole session - arrive,
// credential, watchers - reconciles every registered preview onto it, and then
// waits for it to fail. Reconnection is therefore not a special case: it is
// the same code path as the first connection, which is what makes a manager
// restart survivable (the CLI does the equivalent by re-arriving on
// codes.NotFound; here the whole session is rebuilt).
func (s *Server) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.runSession(ctx); err != nil && ctx.Err() == nil {
			logf("session ended: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
		logf("reconnecting to the manager in %s", s.cfg.reconnectBackoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.reconnectBackoff):
		}
	}
}

// runSession builds one session and blocks until it dies.
func (s *Server) runSession(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	conn, err := grpc.NewClient(s.cfg.managerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer{s.cfg.tokenPath}))
	if err != nil {
		return fmt.Errorf("creating manager client: %w", err)
	}
	defer conn.Close()
	mc := managerrpc.NewManagerClient(conn)

	// Version is one of the few RPCs the manager's auth interceptor skips, so
	// it is the honest reachability check before anything that needs a session.
	vctx, vcancel := context.WithTimeout(ctx, 15*time.Second)
	v, err := mc.Version(vctx, &emptypb.Empty{})
	vcancel()
	if err != nil {
		return fmt.Errorf("manager Version: %w", err)
	}
	logf("manager %s %s at %s", v.GetName(), v.GetVersion(), s.cfg.managerAddr)

	// A client session is bound to the namespace it arrives in, and the
	// manager refuses a namespace it does not manage.
	si, err := mc.ArriveAsClient(ctx, &managerrpc.ClientInfo{
		Name:      s.cfg.clientName,
		Namespace: s.cfg.allowedNamespaces[0],
		InstallId: uuid.NewString(),
		Product:   "telepresence",
		Version:   v.GetVersion(),
	})
	if err != nil {
		return fmt.Errorf("ArriveAsClient: %w", err)
	}
	logf("session %s established", si.GetSessionId())

	// A session credential authenticates us to the traffic-agent's own ports.
	// The manager only serves it to the session's owner, and only if it is new
	// enough to mint one at all - so an Unimplemented here is expected against
	// an older manager and must not stop the preview from working.
	token := s.fetchSessionCredential(ctx, mc, si)

	s.mu.Lock()
	s.conn, s.mc, s.session, s.sessionToken = conn, mc, si, token
	s.managerVersion = v.GetName() + " " + v.GetVersion()
	s.connected = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.connected = false
		s.mu.Unlock()
		s.agents.closeAll()
	}()

	// Record the session so a restarted process can depart it.
	s.saveSession(si.GetSessionId())

	errCh := make(chan error, 3)

	go func() { errCh <- s.remainLoop(ctx, mc, si) }()
	go func() { errCh <- s.watchIntercepts(ctx, mc, si) }()
	go func() { errCh <- s.agents.watch(ctx, mc, si) }()

	// Reconcile the registry onto this new session. On a first connection this
	// is a no-op; after a manager restart it re-raises every live preview.
	go s.reraiseAll(ctx)

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// fetchSessionCredential asks the manager for the session-scoped credential
// used against a traffic-agent's gRPC ports. Returns "" when unavailable.
func (s *Server) fetchSessionCredential(ctx context.Context, mc managerrpc.ManagerClient, si *managerrpc.SessionInfo) string {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cred, err := mc.GetSessionCredential(cctx, si)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			logf("GetSessionCredential: not implemented by this manager; agent calls will be unverified " +
				"(permissive agents accept them; an enforcing agent would not)")
		} else {
			logf("GetSessionCredential: %v; agent calls will be unverified", err)
		}
		return ""
	}
	if t := cred.GetToken(); t != "" {
		logf("session credential obtained, %d bytes, expires %s", len(t), cred.GetExpiry().AsTime().Format(time.RFC3339))
		return t
	}
	logf("GetSessionCredential returned no token; agent calls will be unverified")
	return ""
}

// agentMetadata returns the outgoing metadata for a traffic-agent call. The
// session token rides under its own key, which is what makes a WatchDial
// verified; without it the agent accepts the call unverified in permissive
// mode, and an unverified caller can claim a free dial slot but not displace
// an established one.
func (s *Server) agentMetadata(ctx context.Context) context.Context {
	_, _, tok := s.state()
	if tok == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, sessionTokenMetadataKey, tok)
}

// sessionTokenMetadataKey mirrors pkg/sessiontoken.MetadataKey. It is spelled
// out rather than imported so we do not depend on a package that
// only exists in newer manager sources than some clusters run.
const sessionTokenMetadataKey = "x-telepresence-session-token"

// ownConflictRE matches the manager's header-overlap conflict message, which
// names the conflicting intercept as "<sessionId>:<interceptName>" and the
// client that created it. See conflictingOwnSession.
var ownConflictRE = regexp.MustCompile(
	`conflict with intercept ([0-9a-fA-F-]{36}):(\S+).*?created by client "([^"]+)"`)

// conflictingOwnSession reports the session id of a PREVIOUS INCARNATION OF
// THIS SERVICE whose intercept blocks the one we are raising.
//
// This is the orphan sweep that actually works. If the service is killed rather
// than asked to stop, its intercepts stay in manager state owned by a session
// nobody is holding open any more, and the manager refuses an identical
// intercept with "header filters overlap". WatchIntercepts cannot enumerate
// another session's intercepts and ArriveAsClient never re-enters an old
// session, so a restarted process has no other way to find them - but the
// conflict error itself names the session, which is enough to depart it.
//
// Deliberately narrow: it fires ONLY when the blocking intercept was created by
// a client with our own configured name AND belongs to a session other than
// ours. A developer's telepresence session has a different client name, so this
// will never evict a person's intercept; the manager's own ownership check is
// the second gate, since Depart requires the same Principal.
func (s *Server) conflictingOwnSession(err error, currentSession string) (string, bool) {
	m := ownConflictRE.FindStringSubmatch(err.Error())
	if m == nil {
		return "", false
	}
	sessionID, client := m[1], m[3]
	if client != s.cfg.clientName || sessionID == currentSession {
		return "", false
	}
	return sessionID, true
}

// departSession departs a session that is not ours, releasing its intercepts.
func (s *Server) departSession(ctx context.Context, mc managerrpc.ManagerClient, sessionID string) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := mc.Depart(dctx, &managerrpc.SessionInfo{SessionId: sessionID})
	if err != nil && status.Code(err) != codes.NotFound {
		return err
	}
	return nil
}

// remainLoop holds the session open. A NotFound naming our session means the
// manager no longer knows us - it restarted, or expired us - so the session is
// abandoned and Run rebuilds it from scratch.
func (s *Server) remainLoop(ctx context.Context, mc managerrpc.ManagerClient, si *managerrpc.SessionInfo) error {
	t := time.NewTicker(s.cfg.remainInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := mc.Remain(rctx, &managerrpc.RemainRequest{Session: si})
			cancel()
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			if status.Code(err) == codes.NotFound {
				return fmt.Errorf("manager no longer knows session %s: %w", si.GetSessionId(), err)
			}
			logf("Remain: %v", err)
		}
	}
}

// watchIntercepts keeps each preview's reported disposition current. The
// manager filters this stream to our own session's intercepts.
func (s *Server) watchIntercepts(ctx context.Context, mc managerrpc.ManagerClient, si *managerrpc.SessionInfo) error {
	st, err := mc.WatchIntercepts(ctx, si)
	if err != nil {
		return fmt.Errorf("WatchIntercepts: %w", err)
	}
	for {
		snap, err := st.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("WatchIntercepts recv: %w", err)
		}
		for _, ii := range snap.GetIntercepts() {
			s.reg.setStatus(ii.GetSpec().GetName(), ii.GetDisposition().String(), ii.GetMessage())
		}
	}
}

// prepare calls PrepareIntercept, and if the only thing in the way is an
// intercept left behind by a previous incarnation of this service, departs that
// dead session and tries once more. That turns a crashed pod from a header
// that hangs until the 24h client TTL into one that recovers on the next call.
func (s *Server) prepare(
	ctx context.Context, mc managerrpc.ManagerClient, si *managerrpc.SessionInfo, spec *managerrpc.InterceptSpec,
) (*managerrpc.PreparedIntercept, error) {
	for attempt := 0; ; attempt++ {
		pctx, pcancel := context.WithTimeout(ctx, 3*time.Minute)
		pi, err := mc.PrepareIntercept(pctx, &managerrpc.CreateInterceptRequest{Session: si, InterceptSpec: spec})
		pcancel()

		if err == nil && pi.GetError() != "" {
			err = fmt.Errorf("%s", pi.GetError())
		}
		if err == nil {
			return pi, nil
		}
		if attempt > 0 {
			return nil, fmt.Errorf("PrepareIntercept: %w", err)
		}
		stale, ok := s.conflictingOwnSession(err, si.GetSessionId())
		if !ok {
			return nil, fmt.Errorf("PrepareIntercept: %w", err)
		}
		logf("orphan sweep: %s is blocked by our own dead session %s; departing it", spec.Name, stale)
		if derr := s.departSession(ctx, mc, stale); derr != nil {
			return nil, fmt.Errorf("PrepareIntercept: %w (departing stale session %s: %v)", err, stale, derr)
		}
	}
}

// raise creates the manager-side intercept for one preview. Safe to call for a
// preview that is already up: the manager is the authority and a duplicate
// Create for the same name in the same session is reported as such.
func (s *Server) raise(ctx context.Context, p *Preview) error {
	mc, si, _ := s.state()
	if mc == nil || si == nil {
		return fmt.Errorf("no manager session")
	}

	if err := p.resolveTarget(); err != nil {
		return err
	}

	s.raiseMu.Lock()
	defer s.raiseMu.Unlock()

	spec := &managerrpc.InterceptSpec{
		Name:           p.Name,
		Client:         s.cfg.clientName,
		Agent:          p.Workload,
		Namespace:      p.Namespace,
		Mechanism:      "tcp",
		TargetHost:     p.TargetHost,
		TargetPort:     int32(p.TargetPort),
		PortIdentifier: p.PortID,
		// Nil for a global intercept, and that is the whole switch between the
		// two mechanisms. See Preview.headerFilters.
		HeaderFilters: p.headerFilters(),
		// node_agent asks the manager for a standalone Job pinned to the
		// target's node instead of injecting a sidecar, so the target pod is
		// never restarted and is left byte-identical.
		NodeAgent:        true,
		RoundtripLatency: int64(2 * time.Second),
		DialTimeout:      int64(10 * time.Second),
	}

	// Named only when the caller had to: a workload with two Services on one
	// port makes PrepareIntercept refuse with "multiple interceptable services
	// with port N - please specify the service", which is otherwise an error
	// about a flag this API does not have.
	if p.ServiceName != "" {
		spec.ServiceName = p.ServiceName
	}

	// PrepareIntercept is where the node-agent Job is provisioned. It also
	// returns the resolved service/container facts, which must be folded back
	// into the spec before CreateIntercept, exactly as the connector does.
	pi, err := s.prepare(ctx, mc, si, spec)
	if err != nil {
		return err
	}

	if pi.GetServicePort() > 0 || pi.GetServicePortName() != "" {
		spec.ServicePortName = pi.GetServicePortName()
		spec.ServicePort = pi.GetServicePort()
		if pi.GetServicePortName() != "" {
			spec.PortIdentifier = pi.GetServicePortName()
		} else {
			spec.PortIdentifier = strconv.Itoa(int(pi.GetServicePort()))
		}
	}
	spec.Protocol = pi.GetProtocol()
	spec.ContainerPort = pi.GetContainerPort()
	spec.ContainerName = pi.GetContainerName()
	spec.PodPorts = pi.GetPodPorts()
	spec.ServiceName = pi.GetServiceName()
	spec.ServiceUid = pi.GetServiceUid()
	spec.ServiceIps = pi.GetServiceIps()
	spec.WorkloadKind = pi.GetWorkloadKind()

	cctx, ccancel := context.WithTimeout(ctx, 3*time.Minute)
	ii, err := mc.CreateIntercept(cctx, &managerrpc.CreateInterceptRequest{Session: si, InterceptSpec: spec})
	ccancel()
	if err != nil {
		return fmt.Errorf("CreateIntercept: %w", err)
	}

	s.reg.setStatus(p.Name, ii.GetDisposition().String(), ii.GetMessage())
	match := p.HeaderName + ": " + p.HeaderValue
	if p.Global {
		match = "GLOBAL (no header, all traffic to the port)"
	}
	logf("raised %s: %s.%s %s -> %s:%d (%s)",
		p.Name, p.Workload, p.Namespace, match,
		p.TargetHost, p.TargetPort, ii.GetDisposition())

	// The tunnel this workload needs may not exist yet, or may be for a
	// different set of workloads; reconcile now rather than waiting for the
	// next agent-pod snapshot.
	s.agents.reconcileNow()
	return nil
}

// remove tears down the manager-side intercept for one preview.
func (s *Server) remove(ctx context.Context, p *Preview) error {
	mc, si, _ := s.state()
	if mc == nil || si == nil {
		return fmt.Errorf("no manager session")
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var firstErr error
	if _, err := mc.RemoveIntercept(rctx, &managerrpc.RemoveInterceptRequest2{Session: si, Name: p.Name}); err != nil {
		if status.Code(err) != codes.NotFound {
			firstErr = fmt.Errorf("RemoveIntercept %s: %w", p.Name, err)
		}
	}

	// ReleaseAgent lets the manager reap the node-agent Job, but only once no
	// other intercept of this workload is left - another work id may still be
	// previewing the same service.
	if !s.workloadStillPreviewed(p) {
		if _, err := mc.ReleaseAgent(rctx, &managerrpc.ReleaseAgentRequest{
			Session: si, Name: p.Workload, Namespace: p.Namespace,
		}); err != nil && status.Code(err) != codes.NotFound {
			logf("ReleaseAgent %s.%s: %v", p.Workload, p.Namespace, err)
		}
	}

	kind := "work id " + p.WorkID
	if p.Schedule != "" {
		kind = "schedule " + p.Schedule
	}
	logf("removed %s (%s.%s, %s)", p.Name, p.Workload, p.Namespace, kind)
	s.agents.reconcileNow()
	return firstErr
}

// workloadStillPreviewed reports whether any other registered preview still
// intercepts the same workload.
func (s *Server) workloadStillPreviewed(p *Preview) bool {
	for _, o := range s.reg.all() {
		if o.agentKey() == p.agentKey() {
			return true
		}
	}
	return false
}

// reraiseAll reconciles every registered preview onto the current session.
func (s *Server) reraiseAll(ctx context.Context) {
	ps := s.reg.all()
	if len(ps) == 0 {
		return
	}
	logf("re-raising %d preview(s) onto the new session", len(ps))
	for _, p := range ps {
		if ctx.Err() != nil {
			return
		}
		if err := s.raise(ctx, p); err != nil {
			logf("re-raising %s: %v", p.Name, err)
			s.reg.setStatus(p.Name, "ERROR", err.Error())
		}
	}
}

// Shutdown removes every intercept and departs the session. This is the real
// cleanup path: on SIGTERM the manager state is left as clean as we found it.
func (s *Server) Shutdown(ctx context.Context) {
	mc, si, _ := s.state()
	if mc == nil || si == nil {
		return
	}
	for _, p := range s.reg.all() {
		// Drop it from the registry first: remove() consults the registry to
		// decide whether this workload still has another preview and so
		// whether the node-agent may be released. Leaving the entry in place
		// makes that check always say "yes" and the Job is never reaped.
		s.reg.removeService(p.key())
		if err := s.remove(ctx, p); err != nil {
			logf("shutdown: %v", err)
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := mc.Depart(dctx, si); err != nil {
		logf("Depart: %v", err)
	} else {
		logf("departed session %s", si.GetSessionId())
	}
	s.clearSession()
}

// --- orphan sweep -----------------------------------------------------------
//
// If the service is killed rather than asked to stop, the manager-side
// intercept and its node-agent Job linger until the client session expires -
// and requests carrying that intercept's header HANG rather than falling back
// to the live pod. The agent's fail-open branch covers a BROKEN client stream,
// not an ABSENT client: with no dial watcher at all it retries on a constant
// 20ms backoff with no max elapsed time and never reaches the fail-open path.
// Measured, not inferred.
//
// So sweeping an orphan matters to exactly one thing: the header it answers,
// which is a black hole until something clears it. No other traffic is touched
// and nobody gets a wrong answer.
//
// It cannot be done the obvious way. WatchIntercepts is filtered to the calling
// session's own intercepts, and rejects an empty session id outright
// (InvalidArgument, "a session id is required"), so a fresh session cannot
// enumerate a dead one's work. ArriveAsClient always mints a new session id, so
// the predecessor cannot be re-entered either. What does work is remembering
// the session id and departing it: Depart removes the client and everything it
// owned. A pod replacement that loses the recorded id leaves the orphan to the
// manager's own TTL.

func (s *Server) saveSession(id string) {
	if s.cfg.statePath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.statePath), 0o755); err != nil {
		logf("recording session: %v", err)
		return
	}
	if err := os.WriteFile(s.cfg.statePath, []byte(id), 0o600); err != nil {
		logf("recording session: %v", err)
	}
}

func (s *Server) clearSession() {
	if s.cfg.statePath != "" {
		_ = os.Remove(s.cfg.statePath)
	}
}

// SweepPreviousSession departs a session left behind by a previous incarnation.
func (s *Server) SweepPreviousSession(ctx context.Context) {
	if s.cfg.statePath == "" {
		return
	}
	b, err := os.ReadFile(s.cfg.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			logf("reading recorded session: %v", err)
		}
		return
	}
	old := strings.TrimSpace(string(b))
	if old == "" {
		return
	}

	conn, err := grpc.NewClient(s.cfg.managerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearer{s.cfg.tokenPath}))
	if err != nil {
		logf("orphan sweep: %v", err)
		return
	}
	defer conn.Close()

	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = managerrpc.NewManagerClient(conn).Depart(dctx, &managerrpc.SessionInfo{SessionId: old})
	switch {
	case err == nil:
		logf("orphan sweep: departed previous session %s, releasing anything it still held", old)
	case status.Code(err) == codes.NotFound:
		logf("orphan sweep: previous session %s was already gone", old)
	default:
		logf("orphan sweep: departing previous session %s: %v", old, err)
	}
	s.clearSession()
}
