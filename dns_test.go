package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The DNS-redirect kind, against client-go's own fake clientset through the
// real clusterKube, so what these prove is the code that actually runs.
//
// The ConfigMap they point at is a throwaway in a throwaway namespace. That is
// not incidental: the whole reason the target is three env vars rather than a
// constant is that this mode can be proved end to end without a cluster's
// resolver anywhere near it.

const (
	testDNSNamespace = "dns-sandbox"
	testDNSName      = "throwaway-coredns"
	testDNSKey       = "host.override"
)

// preexisting is a hosts block with a line in it that nothing in this service
// wrote. Every test below starts from it, because "does it leave other people's
// lines alone" is half of what is being asked.
const preexisting = `hosts {
    10.1.2.3 someone-elses.example.internal
    fallthrough
}
`

const otherLine = "10.1.2.3 someone-elses.example.internal"

func dnsServer(t *testing.T, content string, schedules ...*schedule) (*Server, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: testDNSNamespace, Name: testDNSName},
		Data:       map[string]string{testDNSKey: content},
	})
	cfg := cfgFor("shop", "previews")
	cfg.schedules = schedules
	cfg.scheduleCheckInterval = 30 * time.Second
	cfg.dnsConfigMapNamespace, cfg.dnsConfigMapName, cfg.dnsConfigMapKey = testDNSNamespace, testDNSName, testDNSKey
	s := &Server{
		cfg:      cfg,
		reg:      newRegistry(),
		sched:    newScheduleStates(),
		over:     newOverrides(),
		sessions: map[string]*mgrSession{},
		saved:    map[string]string{},
		kube:     &clusterKube{cs: cs},
	}
	return s, cs
}

func dnsSchedule(t *testing.T, yaml string) *schedule {
	t.Helper()
	scheds := compile(t, yaml)
	if len(scheds) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(scheds))
	}
	if scheds[0].kind != kindDNS {
		t.Fatalf("schedule compiled as %s, want dns", scheds[0].kind)
	}
	return scheds[0]
}

// sqlOffHours is the shape this kind was built for, with a fictional hostname:
// something that is not a workload, off out of hours, reachable only through
// the resolver.
func sqlOffHours(t *testing.T) *schedule {
	t.Helper()
	return dnsSchedule(t, `
defaultLocation: Europe/London
schedules:
  - name: sql-offhours
    type: dns
    hostname: dev-sql.example.internal
    redirectTo: 10.42.0.9
    windows:
      - days: [Mon, Tue, Wed, Thu]
        start: "18:32"
        end:   "07:21"
`)
}

func data(t *testing.T, cs *fake.Clientset) string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(testDNSNamespace).Get(context.Background(), testDNSName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the ConfigMap back: %v", err)
	}
	return cm.Data[testDNSKey]
}

func at(t *testing.T, sc *schedule, when string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02 15:04", when, sc.loc)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestDNSWindowOpensClosesAndCoexists is the acceptance bar for this kind. It
// is one sequence rather than three tests because the property being proved is
// about a sequence: what the ConfigMap looks like after an open, after a drift,
// and after a close, with somebody else's line in it throughout.
func TestDNSWindowOpensClosesAndCoexists(t *testing.T) {
	sc := sqlOffHours(t)
	s, cs := dnsServer(t, preexisting, sc)
	ctx := context.Background()

	// 2026-09-14 is a Monday. Outside the window first: nothing is written and
	// the pre-existing line is not touched.
	s.reconcileDNSSchedule(ctx, sc, at(t, sc, "2026-09-14 09:00"))
	if got := data(t, cs); got != preexisting {
		t.Fatalf("a closed window rewrote the ConfigMap:\n%s", got)
	}

	// The window opens.
	s.reconcileDNSSchedule(ctx, sc, at(t, sc, "2026-09-14 18:32"))
	open := data(t, cs)
	want := "10.42.0.9 dev-sql.example.internal # agentic-preview:sql-offhours"
	if !strings.Contains(open, want) {
		t.Fatalf("the open window did not add the tagged line:\n%s", open)
	}
	if !strings.Contains(open, otherLine) {
		t.Fatalf("the open window lost somebody else's line:\n%s", open)
	}
	if !strings.Contains(open, "fallthrough") {
		t.Fatalf("the open window lost the fallthrough:\n%s", open)
	}
	if n := strings.Count(open, "hosts {"); n != 1 {
		t.Fatalf("want the one hosts block that was already there, got %d:\n%s", n, open)
	}
	if st := s.scheduleStateFor(sc.spec.Name); !st.Open || !st.Up || st.Problem != "" || st.Raises != 1 {
		t.Fatalf("state after opening: %+v", st)
	}

	// A second tick inside the window changes nothing: no rewrite, no second
	// line, and no Update call - a controller that writes every tick reloads
	// the cluster's resolver every tick.
	writes := countUpdates(cs)
	s.reconcileDNSSchedule(ctx, sc, at(t, sc, "2026-09-14 19:00"))
	if got := data(t, cs); got != open {
		t.Fatalf("a steady tick rewrote the ConfigMap:\n%s", got)
	}
	if n := countUpdates(cs) - writes; n != 0 {
		t.Fatalf("a steady tick issued %d ConfigMap update(s); want none", n)
	}
	if st := s.scheduleStateFor(sc.spec.Name); st.ReRaises != 0 {
		t.Fatalf("a steady tick counted a re-raise: %+v", st)
	}

	// DRIFT. Something else rewrites the ConfigMap and drops our line - a flux
	// reconcile of the real manifest, an operator reasserting its copy, a
	// colleague with kubectl. The window is still open, so the next tick must
	// notice, count it, and put the line back.
	overwrite(t, cs, preexisting)
	s.reconcileDNSSchedule(ctx, sc, at(t, sc, "2026-09-14 19:30"))
	after := data(t, cs)
	if !strings.Contains(after, want) {
		t.Fatalf("the drift check did not re-apply the line:\n%s", after)
	}
	if !strings.Contains(after, otherLine) {
		t.Fatalf("the drift check lost somebody else's line:\n%s", after)
	}
	st := s.scheduleStateFor(sc.spec.Name)
	if st.ReRaises != 1 {
		t.Fatalf("want reRaises 1 after drift, got %d - a climbing count is the signal that something is "+
			"fighting the controller, so not counting it is the whole failure", st.ReRaises)
	}
	if !st.Up || st.Problem != "" {
		t.Fatalf("state after a recovered drift: %+v", st)
	}

	// A line that is PRESENT but WRONG is drift too: an address that is not
	// ours is not our redirect, however tagged it is.
	overwrite(t, cs, strings.Replace(after, "10.42.0.9", "10.42.0.99", 1))
	s.reconcileDNSSchedule(ctx, sc, at(t, sc, "2026-09-14 20:00"))
	if got := data(t, cs); !strings.Contains(got, want) {
		t.Fatalf("the drift check did not correct a wrong address:\n%s", got)
	}
	if st := s.scheduleStateFor(sc.spec.Name); st.ReRaises != 2 {
		t.Fatalf("want reRaises 2 after a second drift, got %d", st.ReRaises)
	}

	// The window closes: exactly our line goes, and nothing else.
	s.reconcileDNSSchedule(ctx, sc, at(t, sc, "2026-09-15 07:21"))
	closed := data(t, cs)
	if strings.Contains(closed, "agentic-preview:sql-offhours") {
		t.Fatalf("the closed window left the line behind:\n%s", closed)
	}
	if closed != preexisting {
		t.Fatalf("closing did not restore the block to what it was before this schedule touched it:\nwant:\n%s\ngot:\n%s",
			preexisting, closed)
	}
	if st := s.scheduleStateFor(sc.spec.Name); st.Open || st.Up || st.Problem != "" {
		t.Fatalf("state after closing: %+v", st)
	}
}

// TestDNSSchedulesShareAHostsBlock is the same coexistence property between two
// of this service's own schedules: each owns its tagged line and neither reads
// or rewrites the other's.
func TestDNSSchedulesShareAHostsBlock(t *testing.T) {
	scheds := compile(t, `
schedules:
  - name: one
    type: dns
    hostname: first.example.internal
    redirectTo: 10.0.0.1
    windows: [{days: [daily], start: "00:00", end: "24:00"}]
  - name: two
    type: dns
    hostname: second.example.internal
    redirectTo: 10.0.0.2
    windows: [{days: [daily], start: "00:00", end: "24:00"}]
`)
	s, cs := dnsServer(t, preexisting, scheds...)
	ctx, now := context.Background(), time.Now()

	s.reconcileSchedules(ctx, now)
	both := data(t, cs)
	for _, want := range []string{
		"10.0.0.1 first.example.internal # agentic-preview:one",
		"10.0.0.2 second.example.internal # agentic-preview:two",
		otherLine,
	} {
		if !strings.Contains(both, want) {
			t.Fatalf("want %q in:\n%s", want, both)
		}
	}

	// Close the first one only, by forcing it shut on demand - which is also
	// the override path, reconciled by the same loop.
	if _, err := s.SetOverride("one", overrideClosed, "test", 0, now); err != nil {
		t.Fatal(err)
	}
	s.reconcileSchedules(ctx, now)
	left := data(t, cs)
	if strings.Contains(left, "agentic-preview:one") {
		t.Fatalf("the forced-closed schedule kept its line:\n%s", left)
	}
	if !strings.Contains(left, "agentic-preview:two") || !strings.Contains(left, otherLine) {
		t.Fatalf("closing one schedule took another line with it:\n%s", left)
	}
}

// TestDNSPermissionFailureAlarms is the live-environment gap this mode is most
// likely to meet: the ConfigMap is usually in kube-system, which is not in
// ALLOWED_NAMESPACES and which the shipped RBAC does not grant. That must be a
// readable `problem`, not a panic and not a crash loop - a pod that crash-loops
// over one schedule's RBAC takes every other schedule down with it.
func TestDNSPermissionFailureAlarms(t *testing.T) {
	sc := sqlOffHours(t)
	s, cs := dnsServer(t, preexisting, sc)
	cs.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "configmaps"}, testDNSName,
			errUnimportant{})
	})

	s.reconcileDNSSchedule(context.Background(), sc, at(t, sc, "2026-09-14 18:32"))

	st := s.scheduleStateFor(sc.spec.Name)
	if !st.Open {
		t.Fatalf("the window should still be open: %+v", st)
	}
	if st.Up {
		t.Fatalf("a redirect that could not be written must not report itself up: %+v", st)
	}
	if !strings.Contains(st.Problem, "not permitted") || !strings.Contains(st.Problem, "get and update") {
		t.Fatalf("problem does not say what is missing: %q", st.Problem)
	}
	if got := data(t, cs); got != preexisting {
		t.Fatalf("a refused write still changed the ConfigMap:\n%s", got)
	}
}

// TestDNSMissingConfigMapAlarms is the other environment gap: pointed at a
// ConfigMap that is not there. This mode never creates one - a resolver
// ConfigMap invented by a preview tool is not a thing anybody asked for.
func TestDNSMissingConfigMapAlarms(t *testing.T) {
	sc := sqlOffHours(t)
	s, _ := dnsServer(t, preexisting, sc)
	s.cfg.dnsConfigMapName = "not-there"

	s.reconcileDNSSchedule(context.Background(), sc, at(t, sc, "2026-09-14 18:32"))

	st := s.scheduleStateFor(sc.spec.Name)
	if st.Up || !strings.Contains(st.Problem, "does not exist") {
		t.Fatalf("want a readable problem about the missing ConfigMap, got up=%v problem=%q", st.Up, st.Problem)
	}
}

// TestDNSFirstTickSweepsAStaleLine covers the restart case: the pod was killed
// while a window was open, so a redirect nobody has declared live is sitting in
// the ConfigMap. The first tick of the replacement must take it out, because
// the state this reconciles from is the ConfigMap and not memory.
func TestDNSFirstTickSweepsAStaleLine(t *testing.T) {
	sc := sqlOffHours(t)
	stale := "hosts {\n    10.42.0.9 dev-sql.example.internal # agentic-preview:sql-offhours\n    " +
		otherLine + "\n    fallthrough\n}\n"
	s, cs := dnsServer(t, stale, sc)

	// 09:00 on a Monday: the window is shut.
	s.reconcileDNSSchedule(context.Background(), sc, at(t, sc, "2026-09-14 09:00"))

	got := data(t, cs)
	if strings.Contains(got, "agentic-preview:sql-offhours") {
		t.Fatalf("the first tick left a stale redirect live outside its window:\n%s", got)
	}
	if !strings.Contains(got, otherLine) {
		t.Fatalf("the sweep took somebody else's line with it:\n%s", got)
	}
}

// TestDNSFailedSweepIsRetriedAndAlarms is the failure mode the happy sweep
// above hides. A replacement pod starting into a briefly unavailable API
// server is exactly when the sweep matters and exactly when it fails, and a
// sweep is not a one-shot: while it has not succeeded, every tick of a shut
// window tries again, and the stale redirect it cannot rule out is a `problem`
// on GET /schedules rather than one log line nobody reads.
func TestDNSFailedSweepIsRetriedAndAlarms(t *testing.T) {
	sc := sqlOffHours(t)
	stale := "hosts {\n    10.42.0.9 dev-sql.example.internal # agentic-preview:sql-offhours\n    " +
		otherLine + "\n    fallthrough\n}\n"
	s, cs := dnsServer(t, stale, sc)

	down := true
	cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if down {
			return true, nil, apierrors.NewServiceUnavailable("the API server is not answering")
		}
		return false, nil, nil
	})

	ctx := context.Background()
	// Three shut-window ticks while the API server is unreachable. Every one
	// of them must still be trying, and saying so.
	for i, when := range []string{"2026-09-14 09:00", "2026-09-14 09:01", "2026-09-14 09:02"} {
		s.reconcileDNSSchedule(ctx, sc, at(t, sc, when))
		st := s.scheduleStateFor(sc.spec.Name)
		if st.Problem == "" {
			t.Fatalf("tick %d: a sweep that could not run reported no problem: %+v", i, st)
		}
		if !strings.Contains(st.Problem, "stale redirect") {
			t.Fatalf("tick %d: problem does not say what could not be ruled out: %q", i, st.Problem)
		}
		if st.Up {
			t.Fatalf("tick %d: nothing is up while the window is shut: %+v", i, st)
		}
	}
	// Nothing was written, so the stale line is necessarily still there - the
	// ConfigMap cannot be read back to say so while the reactor is failing
	// reads, which is the point of asserting on the writes instead.
	if n := countUpdates(cs); n != 0 {
		t.Fatalf("a sweep whose read failed still wrote to the ConfigMap %d time(s)", n)
	}

	// The API server comes back. The very next tick finishes the sweep.
	down = false
	s.reconcileDNSSchedule(ctx, sc, at(t, sc, "2026-09-14 09:03"))

	got := data(t, cs)
	if strings.Contains(got, "agentic-preview:sql-offhours") {
		t.Fatalf("the retried sweep left the stale redirect live:\n%s", got)
	}
	if !strings.Contains(got, otherLine) {
		t.Fatalf("the sweep took somebody else's line with it:\n%s", got)
	}
	st := s.scheduleStateFor(sc.spec.Name)
	if st.Problem != "" {
		t.Fatalf("a completed sweep should clear the problem: %q", st.Problem)
	}

	// And it is done: a shut window that has been swept writes nothing more.
	before := countUpdates(cs)
	s.reconcileDNSSchedule(ctx, sc, at(t, sc, "2026-09-14 09:04"))
	if after := countUpdates(cs); after != before {
		t.Fatalf("a swept, shut window wrote to the ConfigMap again: %d writes became %d", before, after)
	}
}

// TestOverrideForcesAWindowAndReleasesIt is the on-demand lever: force it open
// outside its window, force it shut inside, and hand it back. Note what it is
// NOT - a replacement for the loop. A forced-open redirect that drifts is
// re-applied by the same drift check, which is why the override changes only
// desiredOpen and nothing else.
func TestOverrideForcesAWindowAndReleasesIt(t *testing.T) {
	sc := sqlOffHours(t)
	s, cs := dnsServer(t, preexisting, sc)
	ctx := context.Background()
	tag := "agentic-preview:sql-offhours"

	// 09:00 Monday: shut, and nothing written.
	noon := at(t, sc, "2026-09-14 09:00")
	s.reconcileDNSSchedule(ctx, sc, noon)
	if strings.Contains(data(t, cs), tag) {
		t.Fatal("the window is shut and the line is there")
	}

	// Forced open, on demand, outside any window.
	if _, err := s.SetOverride(sc.spec.Name, overrideOpen, "dev SQL went down unplanned", time.Hour, noon); err != nil {
		t.Fatal(err)
	}
	s.reconcileDNSSchedule(ctx, sc, noon)
	if !strings.Contains(data(t, cs), tag) {
		t.Fatal("a forced-open schedule did not write its line")
	}

	// The loop is still the loop: drift while forced open is still re-applied
	// and still counted.
	overwrite(t, cs, preexisting)
	s.reconcileDNSSchedule(ctx, sc, noon.Add(time.Minute))
	if !strings.Contains(data(t, cs), tag) {
		t.Fatal("drift while forced open was not re-applied")
	}
	if st := s.scheduleStateFor(sc.spec.Name); st.ReRaises != 1 {
		t.Fatalf("drift while forced open was not counted: reRaises=%d", st.ReRaises)
	}

	// The override lapses on its own, and the schedule goes back to its
	// windows - which at 10:01 on a Monday means shut.
	s.reconcileDNSSchedule(ctx, sc, noon.Add(61*time.Minute))
	if strings.Contains(data(t, cs), tag) {
		t.Fatal("an expired override left the redirect live")
	}

	// Forced SHUT inside the window is the other direction.
	evening := at(t, sc, "2026-09-14 19:00")
	if _, err := s.SetOverride(sc.spec.Name, overrideClosed, "", 0, evening); err != nil {
		t.Fatal(err)
	}
	s.reconcileDNSSchedule(ctx, sc, evening)
	if strings.Contains(data(t, cs), tag) {
		t.Fatal("a forced-closed schedule kept its redirect through its own window")
	}

	// Handed back: the window decides again, and it is open.
	s.ClearOverride(sc.spec.Name)
	s.reconcileDNSSchedule(ctx, sc, evening)
	if !strings.Contains(data(t, cs), tag) {
		t.Fatal("clearing the override did not hand the schedule back to its window")
	}

	// An override names a schedule that exists, and a state that means
	// something. Neither is inferred.
	if _, err := s.SetOverride("not-a-schedule", overrideOpen, "", 0, evening); err == nil {
		t.Fatal("an override for an unknown schedule was accepted")
	}
	if _, err := s.SetOverride(sc.spec.Name, "maybe", "", 0, evening); err == nil {
		t.Fatal("an override state of \"maybe\" was accepted")
	}
}

// TestHostsLineEditingIsSurgical is the text half on its own, including the
// shapes the ConfigMap can be in that a live test does not reach.
func TestHostsLineEditingIsSurgical(t *testing.T) {
	tag := hostsTag("s")
	line := "10.0.0.1 a.example # " + "agentic-preview:s"

	// An empty key gets a whole block, fallthrough included: without it CoreDNS
	// answers NXDOMAIN for everything the block does not list.
	got, changed := applyTaggedLine("", line, tag)
	if !changed || !strings.Contains(got, "hosts {") || !strings.Contains(got, "fallthrough") {
		t.Fatalf("an empty key did not get a usable block:\n%s", got)
	}

	// Indentation of the block is followed, not imposed.
	block := "\thosts {\n\t\tfallthrough\n\t}\n"
	got, _ = applyTaggedLine(block, line, tag)
	if !strings.Contains(got, "\t\t"+line) {
		t.Fatalf("the inserted line did not follow the block's indentation:\n%q", got)
	}

	// Re-applying the same line is not a change, which is what keeps the
	// controller from writing the ConfigMap every tick.
	if _, changed := applyTaggedLine(got, line, tag); changed {
		t.Fatal("re-applying an identical line reported a change")
	}

	// Removing takes one line and reports whether it was there.
	back, removed := removeTaggedLine(got, tag)
	if !removed || strings.Contains(back, tag) {
		t.Fatalf("remove did not remove: %v\n%s", removed, back)
	}
	if _, removed := removeTaggedLine(back, tag); removed {
		t.Fatal("removing an absent line reported a removal")
	}

	// A line tagged for a DIFFERENT schedule is somebody else's and is not
	// touched by either operation.
	theirs := "hosts {\n    10.9.9.9 b.example # agentic-preview:other\n    fallthrough\n}\n"
	got, _ = applyTaggedLine(theirs, line, tag)
	if !strings.Contains(got, "agentic-preview:other") {
		t.Fatalf("applying ours dropped theirs:\n%s", got)
	}
	got, _ = removeTaggedLine(got, tag)
	if !strings.Contains(got, "agentic-preview:other") {
		t.Fatalf("removing ours dropped theirs:\n%s", got)
	}
}

// TestOverrideArrivesOnThePreviewSurface drives the override the way the
// kubectl plugin does - a POST to this service's own API - rather than by
// calling SetOverride directly, because WHERE it arrives is half of what is
// being claimed. This is the same handler, on the same listener, reached
// through the same service proxy as raising a preview; there is no second door.
func TestOverrideArrivesOnThePreviewSurface(t *testing.T) {
	sc := sqlOffHours(t)
	s, cs := dnsServer(t, preexisting, sc)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	post := func(path, body string) (int, string) {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}

	// No duration: the reconciles below run at a simulated Monday morning,
	// while an override's own expiry is wall-clock. An override with no
	// duration lasts until it is cleared, which is the case being driven here.
	if code, body := post("/schedules/sql-offhours/override", `{"state":"open","reason":"dev SQL is down"}`); code != http.StatusOK {
		t.Fatalf("forcing it open: %d %s", code, body)
	} else if !strings.Contains(body, `"state": "open"`) || !strings.Contains(body, "dev SQL is down") {
		t.Fatalf("the response does not report the override it set: %s", body)
	}

	// And the loop acts on it: 09:00 on a Monday is outside every window.
	s.reconcileDNSSchedule(context.Background(), sc, at(t, sc, "2026-09-14 09:00"))
	if !strings.Contains(data(t, cs), "agentic-preview:sql-offhours") {
		t.Fatal("an override set over the API did not reach the reconcile loop")
	}

	// GET /schedules reports it, for both the person who set it and the next
	// person to wonder why a window is open at 09:00.
	resp, err := http.Get(srv.URL + "/schedules")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	listed, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`"kind": "dns"`, `"override"`, `"state": "open"`, `"hostname": "dev-sql.example.internal"`} {
		if !strings.Contains(string(listed), want) {
			t.Fatalf("GET /schedules does not report %s:\n%s", want, listed)
		}
	}
	// A DNS schedule has no intercept, so it must not be reported as if it had
	// one: an empty workload or port would send somebody looking for it.
	for _, unwanted := range []string{`"workload"`, `"port"`} {
		if strings.Contains(string(listed), unwanted) {
			t.Fatalf("a DNS-only schedule reported the intercept field %s:\n%s", unwanted, listed)
		}
	}

	// Cleared over the same surface, and the redirect goes with it.
	if code, body := post("/schedules/sql-offhours/override", `{"state":"auto"}`); code != http.StatusOK {
		t.Fatalf("clearing: %d %s", code, body)
	}
	s.reconcileDNSSchedule(context.Background(), sc, at(t, sc, "2026-09-14 09:05"))
	if strings.Contains(data(t, cs), "agentic-preview:sql-offhours") {
		t.Fatal("clearing the override left the redirect live outside its window")
	}

	// The two refusals: a schedule that does not exist, and a body that does
	// not mean anything.
	if code, _ := post("/schedules/nope/override", `{"state":"open"}`); code != http.StatusNotFound {
		t.Fatalf("an override for an unknown schedule returned %d, want 404", code)
	}
	if code, _ := post("/schedules/sql-offhours/override", `{"state":"maybe"}`); code != http.StatusBadRequest {
		t.Fatalf("an override state of \"maybe\" returned %d, want 400", code)
	}
	if code, _ := post("/schedules/sql-offhours/override", `{"state":"open","duration":"a fortnight"}`); code != http.StatusBadRequest {
		t.Fatalf("an unparseable duration returned %d, want 400", code)
	}
}

func overwrite(t *testing.T, cs *fake.Clientset, content string) {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(testDNSNamespace).Get(context.Background(), testDNSName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[testDNSKey] = content
	if _, err := cs.CoreV1().ConfigMaps(testDNSNamespace).Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func countUpdates(cs *fake.Clientset) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.Matches("update", "configmaps") {
			n++
		}
	}
	return n
}

// errUnimportant is the "why" of a Forbidden, which the API server fills in and
// nothing here reads.
type errUnimportant struct{}

func (errUnimportant) Error() string { return "RBAC: no rule allows update on configmaps" }
