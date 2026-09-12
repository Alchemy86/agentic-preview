package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// DNS-redirect schedules: the second kind of window.
//
// WHY IT EXISTS. An intercept needs a workload. The traffic-agent is a sidecar
// on a pod, so a schedule of the first kind can only divert something that runs
// in the cluster. The dependency this was built for does not: a managed
// database reached over Private Link has a DNS name, an address, and no
// Deployment, Service or pod anywhere to attach to. There is exactly one
// interception point in the cluster that reaches it - the resolver - and this
// is the mode that takes it.
//
// WHAT IT WRITES. One line, into one key of one ConfigMap:
//
//	10.42.0.9 dev-sql.example.internal # agentic-preview:sql-offhours
//
// inside a CoreDNS `hosts { ... fallthrough }` block. The trailing comment is
// the whole ownership model: it is a Corefile comment, so CoreDNS ignores it,
// and it is what makes the line unambiguously THIS schedule's. Every operation
// below is keyed on it, which is what lets a schedule share a hosts block with
// lines nobody here wrote and with other schedules' lines, and remove its own
// without reading or rewriting any of theirs.
//
// WHERE IT WRITES IS CONFIGURATION, not a constant. SCHEDULE_DNS_CONFIGMAP_
// NAMESPACE and _NAME name the ConfigMap and _KEY the key inside it; there is
// no default and a declared DNS schedule without them is fatal at boot. A
// default pointing at kube-system/coredns-custom would make "I wrote a window"
// and "I edited the cluster's resolver" the same act, which is not a thing to
// arrive at by omission.
//
// WHAT IT DOES NOT DO. It never creates the ConfigMap and never rewrites a key
// it was not pointed at. A missing ConfigMap, or an RBAC that does not permit
// the update, is an ALARM and a `problem`, not a crash: a permission gap is a
// live-environment fact to be read off `GET /schedules`, and a pod that
// crash-loops over it takes every OTHER schedule down with it.

// hostsTag is the trailing comment marking a line as one schedule's own.
func hostsTag(name string) string { return "# agentic-preview:" + name }

// hostsLine is the line a DNS schedule owns while its window is open.
func hostsLine(sc *schedule) string {
	return fmt.Sprintf("%s %s %s", sc.redirectTo, sc.hostname, hostsTag(sc.spec.Name))
}

// findTaggedLine returns the tagged line as it currently stands in content, and
// whether it is there at all. The line is returned trimmed, so a comparison
// against hostsLine is about the CONTENT of the entry and not about how it
// happens to be indented.
func findTaggedLine(content, tag string) (string, bool) {
	for _, ln := range strings.Split(content, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasSuffix(t, tag) {
			return t, true
		}
	}
	return "", false
}

// applyTaggedLine puts want into content as the tagged line, and reports
// whether anything changed.
//
// It is a read-modify-write on whatever is actually there: an existing tagged
// line is replaced in place, keeping its position and its indentation, and a
// missing one is inserted at the top of the first hosts block. Only when there
// is no hosts block at all is one written, and then it carries `fallthrough`,
// without which CoreDNS answers NXDOMAIN for every name in the zone that is not
// listed rather than passing it on.
func applyTaggedLine(content, want, tag string) (string, bool) {
	lines := strings.Split(content, "\n")
	for i, ln := range lines {
		if strings.HasSuffix(strings.TrimSpace(ln), tag) {
			if strings.TrimSpace(ln) == want {
				return content, false
			}
			lines[i] = indentOf(ln) + want
			return strings.Join(lines, "\n"), true
		}
	}

	if open, ok := hostsBlockAt(lines); ok {
		out := append([]string{}, lines[:open+1]...)
		indent := blockIndent(lines, open)
		out = append(out, indent+want)
		out = append(out, lines[open+1:]...)
		return strings.Join(out, "\n"), true
	}

	block := "hosts {\n    " + want + "\n    fallthrough\n}\n"
	if strings.TrimSpace(content) == "" {
		return block, true
	}
	return strings.TrimRight(content, "\n") + "\n" + block, true
}

// removeTaggedLine takes the tagged line back out and reports whether it was
// there. Nothing else in the block is read, moved or rewritten - that is the
// point of the tag.
func removeTaggedLine(content, tag string) (string, bool) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	found := false
	for _, ln := range lines {
		if !found && strings.HasSuffix(strings.TrimSpace(ln), tag) {
			found = true
			continue
		}
		out = append(out, ln)
	}
	if !found {
		return content, false
	}
	return strings.Join(out, "\n"), true
}

// hostsBlockAt finds the line opening the first `hosts` block.
func hostsBlockAt(lines []string) (int, bool) {
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if !strings.HasSuffix(t, "{") {
			continue
		}
		if f := strings.Fields(t); len(f) > 0 && f[0] == "hosts" {
			return i, true
		}
	}
	return 0, false
}

// blockIndent is how the block already indents its own entries, so an inserted
// line looks like the ones around it rather than like this program's habits. A
// block with nothing in it yet gets its opening line's indent plus four spaces.
func blockIndent(lines []string, open int) string {
	base := indentOf(lines[open])
	for _, ln := range lines[open+1:] {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if t == "}" {
			break
		}
		if in := indentOf(ln); len(in) > len(base) {
			return in
		}
		break
	}
	return base + "    "
}

func indentOf(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

// --- the reconcile half -----------------------------------------------------

// reconcileDNSSchedule is reconcileSchedule for the DNS kind: the same
// open/check/close shape, against a ConfigMap instead of the manager.
//
// The state it reconciles from is the ConfigMap itself, not memory, so a
// restarted pod re-derives everything on its first tick. That first tick also
// SWEEPS: a window that is shut but whose line is still there - the pod was
// killed mid-window, or the clock moved - has it removed, because a redirect
// nobody declared is live is exactly the failure this mode must not have.
//
// The sweep is keyed on st.swept, not on which tick this is. A replacement pod
// starting into a briefly unavailable API server is exactly the moment the
// sweep is needed and exactly the moment it fails, so one failed attempt must
// not be the only attempt: until a sweep succeeds, every tick of a shut window
// tries again and the failure is a `problem` on GET /schedules meanwhile.
func (s *Server) reconcileDNSSchedule(ctx context.Context, sc *schedule, now time.Time) {
	st := s.scheduleStateFor(sc.spec.Name)
	want, forced := s.desiredOpen(sc, now)

	first := st.Since.IsZero()
	if first || want != st.Open {
		st.Open, st.Since = want, now
		st.NextChange = time.Time{}
		switch {
		case first:
		case want:
			logf("schedule %s: %s - redirecting %s to %s",
				sc.spec.Name, openedBy(forced), sc.hostname, sc.redirectTo)
		default:
			logf("schedule %s: %s", sc.spec.Name, closedBy(forced))
		}
	}
	if st.NextChange.IsZero() || !st.NextChange.After(now) {
		if when, ok := sc.nextChange(now); ok {
			st.NextChange = when
		}
	}

	switch {
	case want && !st.applied:
		s.openDNSWindow(ctx, sc, st, now)
	case want:
		s.checkDNSWindow(ctx, sc, st, now)
	case st.applied || !st.swept:
		s.closeDNSWindow(ctx, sc, st, now, !st.applied)
	default:
		st.Up, st.Problem = false, ""
	}
	s.putScheduleState(sc.spec.Name, st)
}

// openDNSWindow writes the schedule's line, or alarms and says why.
func (s *Server) openDNSWindow(ctx context.Context, sc *schedule, st *scheduleState, now time.Time) {
	changed, err := s.writeHostsLine(ctx, sc, true)
	if err != nil {
		// Not knowing whether the line is there is the same state a fresh pod
		// is in, so the next shut window must sweep rather than assume.
		st.Up, st.applied, st.swept = false, false, false
		s.alarm(sc, st, now, "opening the DNS redirect: "+err.Error())
		return
	}
	st.Up, st.applied, st.swept, st.Problem, st.lastAlarm = true, true, true, "", time.Time{}
	st.raisedAt = now
	st.Raises++
	if changed {
		logf("schedule %s: %s now resolves to %s (%s)", sc.spec.Name, sc.hostname, sc.redirectTo, s.dnsTargetRef())
	} else {
		logf("schedule %s: %s already resolves to %s (%s); adopted it",
			sc.spec.Name, sc.hostname, sc.redirectTo, s.dnsTargetRef())
	}
}

// checkDNSWindow is the drift check, and it is the reason this mode is worth
// piggybacking on the scheduler rather than writing as a pair of CronJobs.
//
// A CronJob writes at 18:32 and is never heard from again. Anything that
// rewrites the ConfigMap in between - a flux reconcile of the real manifest, a
// colleague with kubectl, an operator reasserting its own copy - silently puts
// the name back and the window is open in name only. Here the line is
// re-checked every SCHEDULE_CHECK_INTERVAL, and a ReRaises count that climbs
// means the same thing it means for an intercept: something is fighting the
// controller, and it is winning between ticks.
func (s *Server) checkDNSWindow(ctx context.Context, sc *schedule, st *scheduleState, now time.Time) {
	have, err := s.readHostsLine(ctx, sc)
	if err != nil {
		st.Up = false
		s.alarm(sc, st, now, "checking the DNS redirect: "+err.Error())
		return
	}

	want := hostsLine(sc)
	if have == want {
		if !st.Up {
			logf("schedule %s: DNS redirect for %s recovered", sc.spec.Name, sc.hostname)
		}
		st.Up, st.Problem, st.lastAlarm = true, "", time.Time{}
		return
	}

	was := "is gone from"
	if have != "" {
		was = fmt.Sprintf("reads %q in", have)
	}
	logf("schedule %s: ALARM - the redirect for %s %s %s while the window is open; re-applying it",
		sc.spec.Name, sc.hostname, was, s.dnsTargetRef())
	st.Up, st.ReRaises = false, st.ReRaises+1

	if _, err := s.writeHostsLine(ctx, sc, true); err != nil {
		st.applied, st.swept = false, false
		s.alarm(sc, st, now, "re-applying the DNS redirect: "+err.Error())
		return
	}
	st.applied, st.Up, st.swept, st.Problem, st.lastAlarm = true, true, true, "", time.Time{}
	st.raisedAt = now
}

// closeDNSWindow takes the line back out. sweep marks the cleanup of a line
// this process did not write - a window that was open when the previous pod was
// killed - where finding nothing is the normal case and not news.
//
// A failed sweep is news, though, and it is the one failure that hides a live
// redirect: nothing was removed, nothing is known, and the window may not open
// again for days. So it alarms like every other failure here and leaves swept
// false, which is what brings the next tick back to try again.
func (s *Server) closeDNSWindow(ctx context.Context, sc *schedule, st *scheduleState, now time.Time, sweep bool) {
	changed, err := s.writeHostsLine(ctx, sc, false)
	if err != nil {
		st.Up = false
		if sweep {
			s.alarm(sc, st, now, "checking "+s.dnsTargetRef()+" for a stale redirect of "+
				sc.hostname+": "+err.Error()+" - it may still resolve to "+sc.redirectTo)
			return
		}
		s.alarm(sc, st, now, "closing the DNS redirect: "+err.Error()+
			" - "+sc.hostname+" may still resolve to "+sc.redirectTo)
		return
	}
	st.applied, st.Up, st.swept, st.Problem = false, false, true, ""
	if changed {
		logf("schedule %s: redirect for %s removed from %s; it resolves normally again",
			sc.spec.Name, sc.hostname, s.dnsTargetRef())
	}
}

// --- the ConfigMap half -----------------------------------------------------

// dnsTargetRef is the configured ConfigMap and key, for a log line.
func (s *Server) dnsTargetRef() string {
	return fmt.Sprintf("%s/%s[%s]", s.cfg.dnsConfigMapNamespace, s.cfg.dnsConfigMapName, s.cfg.dnsConfigMapKey)
}

// readHostsLine returns this schedule's line as the ConfigMap currently has it,
// trimmed, or "" when it is not there.
func (s *Server) readHostsLine(ctx context.Context, sc *schedule) (string, error) {
	if s.kube == nil {
		return "", fmt.Errorf("no Kubernetes API client: %w", s.kubeErr)
	}
	cm, err := s.kube.getConfigMap(ctx, s.cfg.dnsConfigMapNamespace, s.cfg.dnsConfigMapName)
	if err != nil {
		return "", dnsConfigMapErr(s.dnsTargetRef(), err)
	}
	have, _ := findTaggedLine(cm.Data[s.cfg.dnsConfigMapKey], hostsTag(sc.spec.Name))
	return have, nil
}

// writeHostsLine is the whole read-modify-write: get the ConfigMap, edit the
// one key by the one tag, and put it back only when that changed something.
//
// Not writing an unchanged ConfigMap is not an optimisation. Every write bumps
// the resourceVersion and wakes everything watching it, including the kubelet
// remounting it into CoreDNS; a controller that writes every 30s whether or not
// anything differs is one that reloads the cluster's resolver every 30s.
func (s *Server) writeHostsLine(ctx context.Context, sc *schedule, present bool) (bool, error) {
	if s.kube == nil {
		return false, fmt.Errorf("no Kubernetes API client: %w", s.kubeErr)
	}
	ref := s.dnsTargetRef()
	cm, err := s.kube.getConfigMap(ctx, s.cfg.dnsConfigMapNamespace, s.cfg.dnsConfigMapName)
	if err != nil {
		return false, dnsConfigMapErr(ref, err)
	}

	key, tag := s.cfg.dnsConfigMapKey, hostsTag(sc.spec.Name)
	var next string
	var changed bool
	if present {
		next, changed = applyTaggedLine(cm.Data[key], hostsLine(sc), tag)
	} else {
		next, changed = removeTaggedLine(cm.Data[key], tag)
	}
	if !changed {
		return false, nil
	}

	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = next
	if _, err := s.kube.updateConfigMap(ctx, cm); err != nil {
		return false, dnsConfigMapErr(ref, err)
	}
	return true, nil
}

// dnsConfigMapErr says what a Kubernetes error actually means for this mode.
// The two that matter are both environment, not config: the ConfigMap is not
// there, or this ServiceAccount may not touch it. Both are a `problem` somebody
// reads off GET /schedules, and neither is worth a crash loop.
func dnsConfigMapErr(ref string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("ConfigMap %s does not exist - this mode never creates it; "+
			"point SCHEDULE_DNS_CONFIGMAP_NAMESPACE/_NAME at one that does: %w", ref, err)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return fmt.Errorf("not permitted to read or write ConfigMap %s: agentic-preview's ServiceAccount "+
			"needs get and update on that ConfigMap in that namespace, which the shipped RBAC does NOT "+
			"grant by default: %w", ref, err)
	default:
		return fmt.Errorf("ConfigMap %s: %w", ref, err)
	}
}
