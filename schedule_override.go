package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// On-demand overrides: forcing one schedule open or closed, now.
//
// A schedule is a recurring window and that is the point of it. But the window
// is a prediction about when a thing is needed, and predictions are wrong: the
// dependency goes down at 14:00 for an unplanned reason, or it is up at 22:00
// and the diversion is in the way of somebody testing against the real one. An
// override is the manual lever for exactly that, the same way a preview can be
// raised on demand rather than waiting for whatever normally raises it.
//
// It is a LAYER ON TOP OF the reconcile loop, never a replacement for it. The
// loop still runs every tick, still health-checks, still drift-checks, still
// alarms; the override changes only the answer to "should this be open now",
// which is one line of it (desiredOpen). So a forced-open intercept that dies
// is re-raised exactly as a scheduled one is, and a forced-open DNS line that
// something overwrites is re-applied exactly as a scheduled one is.
//
// THREE THINGS THAT ARE DELIBERATE:
//
//   - It arrives on the SAME surface as raising a preview: POST to this
//     service's API, which is reachable only through the API server's service
//     proxy (`kubectl agentic-preview`) or from inside the cluster. It is not a
//     new endpoint with a different reachability story, because the property
//     the /schedules docs claim - that a global intercept is not something
//     anything able to reach the Service may conjure - is a property of that
//     surface, and adding a second, laxer door would take it away.
//
//   - It is IN MEMORY and does not survive a restart. The schedule is the
//     durable, reviewable thing and it lives in a file in a repository; an
//     override is an intervention somebody is present for. A pod that restarts
//     returns to what the file says, which is the state somebody agreed to.
//     Anything that needs to outlive a restart is an edit to the file.
//
//   - It can carry an expiry, and `until` is reported. An override with none
//     lasts until it is cleared, which is honest but easy to forget, so it is
//     reported on every GET /schedules for as long as it lasts.

// overrideState is one manual forcing of one schedule.
type overrideState struct {
	// State is "open" or "closed": what the schedule is forced to be,
	// regardless of its windows.
	State string `json:"state"`
	// Since is when the override was set.
	Since time.Time `json:"since,omitzero"`
	// Expires is when it lapses on its own. Zero means it lasts until it is
	// cleared.
	Expires time.Time `json:"expires,omitzero"`
	// Reason is the free-text note the caller gave, if any. It is not parsed;
	// it exists so that the next person to read GET /schedules can see why.
	Reason string `json:"reason,omitempty"`
}

const (
	overrideOpen   = "open"
	overrideClosed = "closed"
	overrideAuto   = "auto"
)

// overrides is the whole store: one per schedule name, at most.
type overrides struct {
	mu sync.Mutex
	by map[string]*overrideState
}

func newOverrides() *overrides { return &overrides{by: map[string]*overrideState{}} }

// SetOverride forces a schedule open or closed. state must be "open", "closed",
// or "auto", which clears it. A zero duration means until cleared.
func (s *Server) SetOverride(name, state, reason string, d time.Duration, now time.Time) (*overrideState, error) {
	sc := s.scheduleNamed(name)
	if sc == nil {
		return nil, fmt.Errorf("no schedule is named %q", name)
	}
	switch strings.ToLower(strings.TrimSpace(state)) {
	case overrideAuto, "clear", "none":
		s.ClearOverride(name)
		return nil, nil

	case overrideOpen, overrideClosed:
	default:
		return nil, fmt.Errorf("state %q is not %q, %q or %q", state, overrideOpen, overrideClosed, overrideAuto)
	}
	if d < 0 {
		return nil, fmt.Errorf("duration %s is negative", d)
	}

	ov := &overrideState{
		State:  strings.ToLower(strings.TrimSpace(state)),
		Since:  now,
		Reason: strings.TrimSpace(reason),
	}
	if d > 0 {
		ov.Expires = now.Add(d)
	}

	s.over.mu.Lock()
	s.over.by[name] = ov
	s.over.mu.Unlock()

	until := "until it is cleared"
	if !ov.Expires.IsZero() {
		until = "until " + ov.Expires.In(sc.loc).Format(time.RFC1123)
	}
	logf("schedule %s: OVERRIDE - forced %s on demand, %s%s. The window itself is unchanged; "+
		"the reconcile loop keeps checking and re-applying as it always does",
		name, ov.State, until, reasonSuffix(ov.Reason))
	return ov, nil
}

// ClearOverride returns a schedule to its declared windows.
func (s *Server) ClearOverride(name string) bool {
	s.over.mu.Lock()
	_, had := s.over.by[name]
	delete(s.over.by, name)
	s.over.mu.Unlock()
	if had {
		logf("schedule %s: override cleared; the declared windows decide again", name)
	}
	return had
}

// overrideFor returns the live override for a schedule, dropping it first if it
// has expired. Expiry is checked on read rather than on a timer of its own: the
// reconcile loop reads this every tick, so an override lapses within one
// SCHEDULE_CHECK_INTERVAL of its expiry, which is the same latency as a window
// edge and bounded by the same setting.
func (s *Server) overrideFor(name string, now time.Time) *overrideState {
	s.over.mu.Lock()
	defer s.over.mu.Unlock()
	ov, ok := s.over.by[name]
	if !ok {
		return nil
	}
	if !ov.Expires.IsZero() && !now.Before(ov.Expires) {
		delete(s.over.by, name)
		logf("schedule %s: override (%s) expired; the declared windows decide again", name, ov.State)
		return nil
	}
	c := *ov
	return &c
}

// desiredOpen is the ONLY place the override changes anything: it answers
// "should this schedule be open now", which the reconcile loop then acts on
// exactly as it acts on a window edge.
func (s *Server) desiredOpen(sc *schedule, now time.Time) (bool, *overrideState) {
	ov := s.overrideFor(sc.spec.Name, now)
	if ov == nil {
		return sc.active(now), nil
	}
	return ov.State == overrideOpen, ov
}

func (s *Server) scheduleNamed(name string) *schedule {
	for _, sc := range s.cfg.schedules {
		if sc.spec.Name == name {
			return sc
		}
	}
	return nil
}

// openedBy and closedBy are the log phrasing, so that a forced change is never
// reported as if the clock had done it.
func openedBy(ov *overrideState) string {
	if ov != nil {
		return "FORCED OPEN on demand"
	}
	return "window OPEN"
}

func closedBy(ov *overrideState) string {
	if ov != nil {
		return "FORCED CLOSED on demand"
	}
	return "window CLOSED"
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}
