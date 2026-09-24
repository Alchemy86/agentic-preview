package main

import (
	"context"
	"fmt"
	"iter"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The expiry safety net, and the two ways a preview outlives it.
//
// The timer itself is not the interesting part: ReapExpired ticks, asks the
// registry which work ids are past their deadline, and tears those down. That
// half has always worked. What it cannot see is the part that leaves previews
// standing for a fortnight, and there are exactly two sources of that:
//
//   - The registry is IN MEMORY. Every preview raised before this process
//     started is invisible to it while the Deployment and Service it created
//     are still running. A pod replacement - a node drain, a chart upgrade, an
//     OOM - therefore does not lose a preview, it makes it IMMORTAL: nothing
//     ever asks it its age again. Measured on a live cluster: 17 of 25 preview
//     Deployments were unknown to the registry, the oldest 310h old under a
//     48h lifetime.
//   - A create that gets as far as building the workload and then fails to
//     RAISE the intercept unregisters itself (handleAdd) and leaves the
//     Deployment and Service it already built behind, with no registry entry
//     and so no deadline. The process need never have restarted.
//
// Both leave the same evidence, and it is evidence the objects carry
// themselves: the managed-by label, the work-id label and a creationTimestamp.
// sweepOrphans reads exactly that, which is why it needs no state of its own
// to survive a restart - the cluster IS the state.
//
// The order of a teardown is the other thing this file is careful about, and
// it is the reverse of what a DELETE does. A DELETE is somebody saying "I am
// finished with this", so dropping its header route first costs nothing. An
// EXPIRY is a timer saying "I think nobody is using this", which is a guess,
// and getting the order wrong turns a wrong guess into a black hole: an
// intercept removed while the preview pod is still running leaves the header
// answered by nothing rather than falling back to the live workload. So expiry
// deletes the objects FIRST, CONFIRMS they are gone, and only then removes the
// intercept. If it cannot confirm, the header route stays exactly where it is
// and the deadline moves out by PREVIEW_REAP_RECHECK - traffic keeps reaching
// the thing that is still running, and the sweep tries again later.

// ReapExpired sweeps previews nothing has touched for PREVIEW_LIFETIME, and
// preview objects whose registry entry no longer exists at all.
//
// It is a safety net and nothing more. Teardown is explicit: a preview goes
// when somebody asks for it to go, and a pull request merging is not somebody
// asking - the change may well still be being tested against the preview after
// it merges. This exists so that a preview somebody forgot does not sit there
// forever, and its deadline is reported by GET /previews before it arrives so
// that a person can see they are about to lose one and extend it.
//
// It can be switched off, and an adopter with something else minding their
// previews should switch it off: a timer that removes a preview while somebody
// is still testing against it is worse than a forgotten pod. It defaults to on
// because that is the safe answer for somebody who never reads the
// configuration. Off means off for the orphan sweep too - an adopter who has
// said "nothing here removes previews on a timer" has said it about previews
// this process has forgotten as much as about the ones it remembers.
func (s *Server) ReapExpired(ctx context.Context) {
	if s.cfg.lifetime <= 0 {
		logf("preview expiry is OFF (PREVIEW_LIFETIME); previews live until somebody deletes them. " +
			"GET /previews reports every preview's age, which is what to watch instead")
		return
	}
	logf("previews expire %s after they are last touched; previews this process does not know about "+
		"are swept %s after they were created, and a teardown that cannot be confirmed is rechecked in %s",
		s.cfg.lifetime, s.cfg.lifetime, s.cfg.reapRecheck)
	tick := time.NewTicker(s.cfg.reapInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, workID := range s.reg.expired(time.Now()) {
			logf("work id %s has not been touched for %s; removing it", workID, s.cfg.lifetime)
			s.expireWork(ctx, workID)
		}
		s.sweepOrphans(ctx)
	}
}

// expireWork is the expiry teardown, and it is deliberately NOT tearDownWork.
//
// tearDownWork drops the intercept first and then the objects, which is right
// for a DELETE: somebody has said they are finished, so the header going quiet
// a moment before the pod does is the correct order and the only order that
// leaves nothing behind if the second half fails.
//
// A timer has no such statement to go on. Removing the intercept from a
// preview whose pod is still running does not restore the live workload - the
// traffic-agent holds the header on an unbounded retry and the header becomes a
// black hole (see the orphan-sweep comment in session.go, which measured it).
// So: delete the objects, confirm they are actually gone, and only then let the
// header route go. Anything short of confirmed leaves the route alone and moves
// the deadline out by PREVIEW_REAP_RECHECK.
func (s *Server) expireWork(ctx context.Context, workID string) {
	nss := s.reg.namespacesOf(workID)

	deleted, problems := s.dropWorkload(ctx, workID, "")

	// The confirmation is scoped to the namespaces this work id's previews are
	// actually in, NOT to every allowed namespace. dropWorkload sweeps them
	// all and reports a list it could not read as a problem, which is normal
	// on a cluster whose RBAC lags its ALLOWED_NAMESPACES; a namespace this
	// work id was never in has no bearing on whether this work id is gone, and
	// treating it as one would stall every expiry on the cluster forever.
	remaining, checkProblems := s.confirmGone(ctx, workID, "", nss)

	if len(remaining) > 0 || len(checkProblems) > 0 {
		until := time.Now().Add(s.cfg.reapRecheck)
		s.reg.touch(workID, until)
		logf("work id %s expiry INCOMPLETE: objects deleted %v, still present %v, problems %v%v. "+
			"Its header route is LEFT IN PLACE - dropping it while the preview is still running would "+
			"hang the header rather than fall back to the live workload. Rechecking at %s",
			workID, deleted, remaining, problems, checkProblems, until.UTC().Format(time.RFC3339))
		return
	}

	// Confirmed gone. Now, and only now, the header route.
	ps := s.reg.removeWork(workID)
	removed := make([]string, 0, len(ps))
	for _, p := range ps {
		if err := s.remove(ctx, p); err != nil {
			problems = append(problems, err.Error())
		}
		removed = append(removed, p.Workload+"."+p.Namespace)
	}
	logf("work id %s expired: objects %v (confirmed gone), intercepts %v, problems %v",
		workID, deleted, removed, problems)
}

// confirmGone reports which of a work id's objects - or one workload's within
// it, when workload is given - are STILL THERE after a delete, and which
// namespaces could not be checked at all.
//
// A delete is accepted asynchronously, so a list taken immediately afterwards
// can still return the object it just removed. It polls rather than asking
// once, because the alternative is a healthy teardown being called incomplete
// and sitting on a 12h recheck for the sake of two seconds. A namespace that
// cannot be listed is returned as a problem and not as "gone": not being
// allowed to look is not evidence of absence, and it is the case where
// dropping the route would be worst.
func (s *Server) confirmGone(ctx context.Context, workID, workload string, namespaces []string) (remaining, problems []string) {
	if s.kube == nil || len(namespaces) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.reapConfirm)
	defer cancel()

	// Backs off rather than polling on a fixed interval. A delete that worked
	// is confirmed on the first pass, so the interval only ever matters when
	// the answer is going to be "not gone" - and a fixed two seconds spends
	// sixty list calls proving it, which is past the Kubernetes client's own
	// rate limiter (20 qps, burst 40, set in kube.go). Measured: it then
	// reports its own throttling as "could not confirm", which is a teardown
	// stalled for twelve hours by nothing at all.
	wait := 500 * time.Millisecond
	selector := ownerSelector(workID, workload)
	for {
		remaining, problems = nil, nil
		for _, ns := range namespaces {
			deps, err := s.kube.listDeployments(ctx, ns, selector)
			if err != nil {
				problems = append(problems, fmt.Sprintf("confirming preview Deployments are gone from %s: %v", ns, err))
			}
			for i := range deps {
				remaining = append(remaining, "deployment/"+deps[i].Name+"."+ns)
			}
			svcs, err := s.kube.listServices(ctx, ns, selector)
			if err != nil {
				problems = append(problems, fmt.Sprintf("confirming preview Services are gone from %s: %v", ns, err))
			}
			for i := range svcs {
				remaining = append(remaining, "service/"+svcs[i].Name+"."+ns)
			}
		}
		if len(remaining) == 0 && len(problems) == 0 {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			sort.Strings(remaining)
			sort.Strings(problems)
			return remaining, problems
		case <-time.After(wait):
		}
		if wait < 10*time.Second {
			wait *= 2
		}
	}
}

// orphanKey is one preview's worth of objects as the CLUSTER describes it,
// read off the labels every object this tool creates carries. It is the same
// triple as a registry serviceKey on purpose: that is what makes "the registry
// has never heard of this" a lookup rather than a guess.
type orphanKey struct {
	namespace string
	workID    string
	workload  string
}

func (k orphanKey) String() string {
	return k.workload + "." + k.namespace + " (work id " + k.workID + ")"
}

// sweepOrphans removes preview objects the registry knows nothing about and
// that are older than PREVIEW_LIFETIME.
//
// It exists because the registry is in memory and the objects are not: a
// preview raised before this process started, or one whose create built the
// workload and then failed to raise the intercept, has no deadline anywhere and
// nothing else will ever look at it again. The evidence it works from is the
// object's own managed-by and work-id labels and its creationTimestamp, so it
// needs no state that a restart could lose.
//
// Two rules keep it from ever touching a live preview:
//
//   - An object whose (namespace, work id, workload) IS in the registry is
//     never an orphan, whatever its age. The normal deadline owns it.
//   - An object has to be reported unknown on TWO consecutive sweeps before
//     anything is deleted. A registry is empty for a moment at startup and the
//     schedule controller re-registers into it asynchronously; one pass is not
//     evidence, and the confirmation costs one reap interval.
//
// There is no intercept to remove alongside the objects and no way to remove
// one: WatchIntercepts is filtered to the calling session's own work and
// ArriveAsClient always mints a new session, so an intercept raised by a
// previous incarnation is unreachable by name from this one. It is already
// gone in practice - SweepPreviousSession departs the recorded session at
// startup, which releases everything it held - and where that record was lost,
// the manager's own TTL is what ends it. Deleting the objects is the half this
// process can do, and the half that is actually accumulating.
func (s *Server) sweepOrphans(ctx context.Context) {
	if s.kube == nil {
		return
	}
	now := time.Now()
	selector := managedByLabel + "=" + managedByValue

	found := map[orphanKey]time.Time{}
	for _, ns := range s.cfg.allowedNamespaces {
		deps, err := s.kube.listDeployments(ctx, ns, selector)
		if err != nil {
			s.orphanListProblem(ns, "Deployments", err, now)
		}
		for i := range deps {
			note(found, ns, deps[i].Labels, deps[i].CreationTimestamp)
		}
		svcs, err := s.kube.listServices(ctx, ns, selector)
		if err != nil {
			s.orphanListProblem(ns, "Services", err, now)
		}
		for i := range svcs {
			note(found, ns, svcs[i].Labels, svcs[i].CreationTimestamp)
		}
	}

	// At most this many teardowns per sweep. Each one deletes and then polls
	// until it can confirm, and a cluster that has accumulated orphans for a
	// fortnight presents them all at once - sixteen of them, measured. Working
	// through the backlog a few at a time keeps one sweep bounded and keeps the
	// client's own rate limiter out of the confirmation, which would otherwise
	// report a perfectly good teardown as unconfirmable. The sweep runs every
	// reap interval, so a backlog clears in minutes either way.
	const budget = 5
	spent := 0

	seen := map[orphanKey]bool{}
	for k, created := range keysInOrder(found) {
		if _, tracked := s.reg.get(serviceKey{WorkID: k.workID, Namespace: k.namespace, Workload: k.workload}); tracked {
			continue
		}
		if created.IsZero() || now.Sub(created) < s.cfg.lifetime {
			continue
		}
		seen[k] = true
		if !s.orphanSeen[k] {
			// First sighting. Say so - an orphan is a fault somewhere else
			// (this process was restarted, or a create half-failed) and the
			// only place it is ever visible is here.
			logf("orphan preview %s has been up %s with no registry entry and no deadline; "+
				"confirming on the next sweep before removing it", k, since(now, created))
			continue
		}
		if next, held := s.orphanNextTry[k]; held && now.Before(next) {
			continue
		}
		if spent >= budget {
			continue
		}
		spent++
		logf("orphan preview %s is %s old, past the %s lifetime, and nothing knows about it; removing it",
			k, since(now, created), s.cfg.lifetime)
		deleted, problems := s.dropWorkload(ctx, k.workID, k.workload)
		// Narrowed to this one workload, not the whole work id: three services
		// of one id go stale together and are swept as three orphans, and
		// confirming on the id would have each of them report its siblings as
		// "still there" and back off for twelve hours over its own success.
		remaining, checkProblems := s.confirmGone(ctx, k.workID, k.workload, []string{k.namespace})
		if len(remaining) > 0 || len(checkProblems) > 0 {
			s.orphanNextTry[k] = now.Add(s.cfg.reapRecheck)
			logf("orphan preview %s NOT removed: deleted %v, still present %v, problems %v%v; retrying in %s",
				k, deleted, remaining, problems, checkProblems, s.cfg.reapRecheck)
			continue
		}
		delete(s.orphanNextTry, k)
		logf("orphan preview %s removed: %v (confirmed gone), problems %v", k, deleted, problems)
	}

	// Anything that stopped being an orphan - deleted, or adopted by a new
	// registry entry - loses its sighting, so a later reappearance is
	// confirmed again from scratch rather than acted on immediately.
	s.orphanSeen = seen
	for k := range s.orphanNextTry {
		if !seen[k] {
			delete(s.orphanNextTry, k)
		}
	}
}

// keysInOrder is found in oldest-first order, so a sweep working through a
// backlog under its budget takes the previews that have been up longest first
// and the order two runs see is the same.
func keysInOrder(found map[orphanKey]time.Time) iter.Seq2[orphanKey, time.Time] {
	ks := make([]orphanKey, 0, len(found))
	for k := range found {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if !found[ks[i]].Equal(found[ks[j]]) {
			return found[ks[i]].Before(found[ks[j]])
		}
		return ks[i].String() < ks[j].String()
	})
	return func(yield func(orphanKey, time.Time) bool) {
		for _, k := range ks {
			if !yield(k, found[k]) {
				return
			}
		}
	}
}

// note records the oldest creationTimestamp seen for one preview's objects. The
// Deployment and its Service are made together but not atomically, and the
// older of the two is the one that says how long this preview has been up.
func note(found map[orphanKey]time.Time, ns string, labels map[string]string, created metav1.Time) {
	workID, workload := labels[workIDLabel], labels[workloadLabel]
	if workID == "" || workload == "" {
		// Carries our managed-by label but not the labels a teardown selects
		// on. Nothing here can name it safely, so nothing here touches it.
		return
	}
	k := orphanKey{namespace: ns, workID: workID, workload: workload}
	if at, ok := found[k]; !ok || created.Time.Before(at) {
		found[k] = created.Time
	}
}

// orphanListProblem reports a namespace the sweep cannot read, at most once per
// recheck interval per namespace. It is throttled because the sweep runs every
// minute and the usual cause is an RBAC Role that has not caught up with
// ALLOWED_NAMESPACES - a standing condition, not an event, and one that must
// stay visible without burying everything else in the log.
func (s *Server) orphanListProblem(ns, kind string, err error, now time.Time) {
	k := ns + "/" + kind
	if next, held := s.orphanQuiet[k]; held && now.Before(next) {
		return
	}
	s.orphanQuiet[k] = now.Add(s.cfg.reapRecheck)
	logf("orphan sweep cannot list preview %s in %s, so previews there can NEVER be swept: %v", kind, ns, err)
}

// since is an age in the same words GET /previews uses.
func since(now, at time.Time) string { return now.Sub(at).Round(time.Second).String() }
