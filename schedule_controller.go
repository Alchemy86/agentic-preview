package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	managerrpc "github.com/telepresenceio/telepresence/rpc/v2/manager"
)

// alarmInterval is how often a refusal that is still refusing says so again.
// The first one is logged the moment it happens; this is so that a schedule
// which has been blocked all night is still saying so in the morning, without
// one line every 30 seconds in between.
const alarmInterval = 5 * time.Minute

// raiseGrace is how long after a successful CreateIntercept a WAITING
// disposition is tolerated before it is treated as a failure to re-raise. It is
// normal for a second or two while the node-agent Job comes up.
const raiseGrace = 90 * time.Second

// scheduleState is the controller's own memory of one schedule. It is separate
// from the Preview in the registry because it is about the CONTROLLER's
// attempts, not about the intercept: how long the current attempt has been
// failing is the thing worth reporting when a window will not open.
type scheduleState struct {
	// Open is whether the window is open right now.
	Open bool `json:"open"`
	// Up is whether the intercept is believed live in manager state.
	Up bool `json:"up"`
	// Since is when Open last changed.
	Since time.Time `json:"since,omitzero"`
	// NextChange is when the window next opens or closes.
	NextChange time.Time `json:"nextChange,omitzero"`
	// Problem is why the intercept is not up while the window is open. It is
	// the field to look at: empty is the only good value during a window.
	Problem string `json:"problem,omitempty"`
	// Raises counts successful CreateIntercepts, ReRaises the ones that
	// followed a health check finding the intercept gone. A ReRaise count that
	// climbs is the signal that something is killing intercepts underneath us.
	Raises   int `json:"raises"`
	ReRaises int `json:"reRaises"`

	raisedAt  time.Time
	lastAlarm time.Time
}

// ScheduleStatus is one schedule as the API reports it.
type ScheduleStatus struct {
	Name      string `json:"name"`
	Workload  string `json:"workload"`
	Namespace string `json:"namespace"`
	Port      string `json:"port"`
	Target    string `json:"target"`
	Windows   string `json:"windows"`
	scheduleState
}

// RunSchedules is the whole scheduled-intercept controller: one loop that both
// opens and closes windows AND health-checks its own live intercepts, because
// those are the same question asked of the same two facts - should it be up, and
// is it up.
//
// Everything it does is a reconcile from (now, manager state) to desired, so
// there is no state to lose: a restarted pod re-derives the window on its first
// tick, which is the same path a window opening takes. That is deliberate and it
// is the answer to the forced-kill case. A kill -9 leaves a global intercept in
// manager state hanging every request to the workload; the replacement pod
// departs that dead session in SweepPreviousSession before it arrives, which
// drops the intercept and lets traffic fall straight through, and then this loop
// raises it again on its first tick. Seconds, not the PREVIEW_LIFETIME sweep.
func (s *Server) RunSchedules(ctx context.Context) {
	if len(s.cfg.schedules) == 0 {
		logf("no scheduled intercepts declared (SCHEDULE_FILE unset); nothing raises an intercept but a POST /previews")
		return
	}

	logf("%d scheduled intercept(s), checked every %s:", len(s.cfg.schedules), s.cfg.scheduleCheckInterval)
	now := time.Now()
	for _, sc := range s.cfg.schedules {
		logf("  %s", sc.describe())
		when, ok := sc.nextChange(now)
		verb := "opens"
		if sc.active(now) {
			verb = "closes"
		}
		if ok {
			logf("    window is %s now; %s %s (in %s)",
				map[bool]string{true: "OPEN", false: "closed"}[sc.active(now)],
				verb, when.In(sc.loc).Format(time.RFC1123), when.Sub(now).Round(time.Minute))
		} else {
			logf("    window is %s now and does not change within a fortnight - check the days", //nolint:goconst // one message
				map[bool]string{true: "OPEN", false: "closed"}[sc.active(now)])
		}
	}

	tick := time.NewTicker(s.cfg.scheduleCheckInterval)
	defer tick.Stop()
	for {
		s.reconcileSchedules(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Server) reconcileSchedules(ctx context.Context, now time.Time) {
	for _, sc := range s.cfg.schedules {
		if ctx.Err() != nil {
			return
		}
		s.reconcileSchedule(ctx, sc, now)
	}
}

// reconcileSchedule brings one schedule's intercept into line with the clock.
func (s *Server) reconcileSchedule(ctx context.Context, sc *schedule, now time.Time) {
	st := s.scheduleStateFor(sc.spec.Name)
	want := sc.active(now)
	_, held := s.reg.get(sc.key())

	// Since is zero only on the very first tick, where want is not a change to
	// report - the startup log has already said which side of the window we
	// started on.
	if first := st.Since.IsZero(); first || want != st.Open {
		st.Open, st.Since = want, now
		st.NextChange = time.Time{}
		switch {
		case first:
		case want:
			logf("schedule %s: window OPEN - raising a global intercept on %s.%s",
				sc.spec.Name, sc.spec.Workload, sc.spec.Namespace)
		default:
			logf("schedule %s: window CLOSED", sc.spec.Name)
		}
	}
	if st.NextChange.IsZero() || !st.NextChange.After(now) {
		if when, ok := sc.nextChange(now); ok {
			st.NextChange = when
		}
	}

	switch {
	case want && !held:
		s.openWindow(ctx, sc, st, now)
	case want && held:
		s.checkWindow(ctx, sc, st, now)
	case !want && held:
		s.closeWindow(ctx, sc, st)
	default:
		st.Up, st.Problem = false, ""
	}
	s.putScheduleState(sc.spec.Name, st)
}

// openWindow raises the global intercept, or refuses to and says why.
func (s *Server) openWindow(ctx context.Context, sc *schedule, st *scheduleState, now time.Time) {
	s.mu.RLock()
	connected := s.connected
	s.mu.RUnlock()
	if !connected {
		st.Up = false
		s.alarm(sc, st, now, "no manager session yet; will retry")
		return
	}

	p := sc.preview()

	// THE REFUSAL. A header-keyed preview already on this workload has put the
	// traffic-agent's port into HTTP mode, where an intercept with no filters
	// qualifies for neither matching tier and is never selected - so raising
	// this would appear to succeed, report ACTIVE, and divert nothing. Refusing
	// is the only honest option, and it has to be loud: there is nobody watching
	// a 03:00 window, so the log line is the whole alarm.
	if other := s.reg.conflictingMode(p); other != nil {
		st.Up = false
		s.alarm(sc, st, now, fmt.Sprintf(
			"REFUSING to open: %s.%s already carries the header-keyed preview %q (work id %s, %s: %s). "+
				"A workload runs in ONE intercept mode at a time: with a header filter live the agent's port is in HTTP mode, "+
				"and this headerless intercept would match nothing while reporting itself healthy. "+
				"Remove that preview (DELETE /previews/%s) or take this workload off the schedule",
			sc.spec.Workload, sc.spec.Namespace, other.Name, other.WorkID,
			other.HeaderName, other.HeaderValue, other.WorkID))
		return
	}

	// Registered before it is raised, exactly as a preview is, so a session
	// lost mid-raise leaves the reconciler something to re-raise.
	if err := s.reg.add(p); err != nil {
		st.Up = false
		s.alarm(sc, st, now, "registering: "+err.Error())
		return
	}
	if err := s.raise(ctx, p); err != nil {
		s.reg.removeService(p.key())
		st.Up = false
		s.alarm(sc, st, now, "raising: "+err.Error())
		return
	}
	st.Up, st.Problem, st.lastAlarm = true, "", time.Time{}
	st.raisedAt = now
	st.Raises++
	logf("schedule %s: global intercept %s is up on %s.%s port %s -> %s:%d (ALL traffic to that port, no header)",
		sc.spec.Name, p.Name, p.Workload, p.Namespace, p.PortID, p.TargetHost, p.TargetPort)
}

// checkWindow is the self-health check, and it is the reason this loop ticks at
// 30 seconds rather than once a window.
//
// A global intercept that dies quietly does not fail open: it takes every
// request to the workload with it and they HANG (docs/LIMITS.md). The existing
// safety nets are both too slow for that - PREVIEW_LIFETIME is hours, and the
// conflict-driven orphan sweep only fires when something tries to raise the same
// intercept again. So this asks the manager directly, every tick, whether the
// intercept it was told to create still exists, and re-raises it if it does not.
func (s *Server) checkWindow(ctx context.Context, sc *schedule, st *scheduleState, now time.Time) {
	p, ok := s.reg.get(sc.key())
	if !ok {
		return
	}
	mc, si, _ := s.state()
	if mc == nil || si == nil {
		st.Up = false
		s.alarm(sc, st, now, "manager session is gone; the intercept will be re-raised when it returns")
		return
	}

	gctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ii, err := mc.GetIntercept(gctx, &managerrpc.GetInterceptRequest{Session: si, Name: p.Name})
	cancel()

	switch {
	case status.Code(err) == codes.NotFound:
		// Gone from manager state while we still think it is up. Nothing is
		// hanging in this direction - an intercept the manager has forgotten is
		// not intercepting - but the window is open and traffic is reaching the
		// real workload, so re-raise rather than wait for anything.
		logf("schedule %s: ALARM - intercept %s has vanished from manager state; re-raising", sc.spec.Name, p.Name)
		s.reg.removeService(p.key())
		st.Up, st.ReRaises = false, st.ReRaises+1
		s.openWindow(ctx, sc, st, now)
		return
	case err != nil:
		if ctx.Err() != nil {
			return
		}
		s.alarm(sc, st, now, "GetIntercept: "+err.Error())
		return
	}

	disp := ii.GetDisposition()
	s.reg.setStatus(p.Name, disp.String(), ii.GetMessage())

	switch {
	case disp == managerrpc.InterceptDispositionType_ACTIVE:
		// Active but with no dial loop established is precisely the shape that
		// hangs: the agent has an intercept to serve and nothing to serve it
		// to. It is normal for a moment after raising while the agent pool
		// reconciles, and an alarm after that.
		if p.AgentPod == "" && now.Sub(st.raisedAt) > raiseGrace {
			st.Up = false
			s.alarm(sc, st, now, fmt.Sprintf(
				"intercept %s is ACTIVE but no node-agent tunnel is established: requests to %s.%s are being "+
					"held rather than answered. Check the node-agent Job", p.Name, p.Workload, p.Namespace))
			return
		}
		if !st.Up {
			logf("schedule %s: intercept %s recovered (ACTIVE)", sc.spec.Name, p.Name)
		}
		st.Up, st.Problem, st.lastAlarm = true, "", time.Time{}

	case disp == managerrpc.InterceptDispositionType_WAITING && now.Sub(st.raisedAt) <= raiseGrace:
		st.Up = true // still coming up; the node-agent Job takes a moment

	default:
		logf("schedule %s: ALARM - intercept %s is %s (%s); tearing it down and raising it again",
			sc.spec.Name, p.Name, disp, ii.GetMessage())
		s.reg.removeService(p.key())
		if rerr := s.remove(ctx, p); rerr != nil {
			logf("schedule %s: removing the unhealthy intercept: %v", sc.spec.Name, rerr)
		}
		st.Up, st.ReRaises = false, st.ReRaises+1
		s.openWindow(ctx, sc, st, now)
	}
}

// closeWindow drops the intercept the same way a graceful stop does, which is
// the whole safety property: RemoveIntercept in a live session leaves nothing in
// manager state, so traffic falls straight through to the real workload.
func (s *Server) closeWindow(ctx context.Context, sc *schedule, st *scheduleState) {
	p, ok := s.reg.removeService(sc.key())
	if !ok {
		return
	}
	if err := s.remove(ctx, p); err != nil {
		// Say it and leave the entry out: the next tick finds the window shut
		// and nothing held, and the orphan sweep covers what the manager kept.
		logf("schedule %s: ALARM - closing %s: %v - traffic to %s.%s may be held until the manager expires it",
			sc.spec.Name, p.Name, err, p.Workload, p.Namespace)
		st.Problem = "closing: " + err.Error()
		st.Up = false
		return
	}
	st.Up, st.Problem = false, ""
	logf("schedule %s: global intercept %s removed; %s.%s is serving its own traffic again",
		sc.spec.Name, p.Name, p.Workload, p.Namespace)
}

// alarm records why a window is not up and logs it, the first time and then
// every alarmInterval for as long as it lasts.
func (s *Server) alarm(sc *schedule, st *scheduleState, now time.Time, problem string) {
	first := st.Problem != problem
	st.Problem = problem
	if first || now.Sub(st.lastAlarm) >= alarmInterval {
		st.lastAlarm = now
		logf("schedule %s: %s", sc.spec.Name, problem)
	}
}

// --- state, for the API and for the loop's own memory -----------------------

type scheduleStates struct {
	mu sync.Mutex
	by map[string]*scheduleState
}

func newScheduleStates() *scheduleStates {
	return &scheduleStates{by: map[string]*scheduleState{}}
}

func (s *Server) scheduleStateFor(name string) *scheduleState {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	if st, ok := s.sched.by[name]; ok {
		c := *st
		return &c
	}
	return &scheduleState{}
}

func (s *Server) putScheduleState(name string, st *scheduleState) {
	s.sched.mu.Lock()
	defer s.sched.mu.Unlock()
	c := *st
	s.sched.by[name] = &c
}

// ScheduleStatuses is every declared schedule with what the controller last
// made of it, for GET /schedules.
func (s *Server) ScheduleStatuses() []ScheduleStatus {
	out := make([]ScheduleStatus, 0, len(s.cfg.schedules))
	for _, sc := range s.cfg.schedules {
		st := s.scheduleStateFor(sc.spec.Name)
		out = append(out, ScheduleStatus{
			Name:      sc.spec.Name,
			Workload:  sc.spec.Workload,
			Namespace: sc.spec.Namespace,
			Port:      sc.portID,
			Target:    fmt.Sprintf("%s:%d", sc.target(), sc.targetPort),
			Windows:   sc.describe(),
			//nolint:govet // a copy of the state is exactly what is wanted here
			scheduleState: *st,
		})
	}
	return out
}
