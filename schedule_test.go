package main

import (
	"strings"
	"testing"
	"time"
)

// london is the zone the measured off-hours window below is written in. It is
// loaded from the embedded tzdata schedule.go pulls in, so this test also
// proves that import is doing its job.
func london(t *testing.T) *time.Location {
	t.Helper()
	l, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("Europe/London: %v - the time/tzdata import in schedule.go is missing", err)
	}
	return l
}

func compile(t *testing.T, yaml string, namespaces ...string) []*schedule {
	t.Helper()
	if len(namespaces) == 0 {
		namespaces = []string{"shop", "previews"}
	}
	path := writeTemp(t, yaml)
	scheds, err := loadSchedules(path, cfgFor(namespaces...))
	if err != nil {
		t.Fatalf("loadSchedules: %v", err)
	}
	return scheds
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := t.TempDir() + "/schedules.yaml"
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMeasuredOffHoursWindow is the schema's reason for existing, written out
// as the real window it has to express: the estate's database goes at 18:32 and
// returns at 07:21 UK local on weekdays, and stays down from Friday evening
// right through to Monday morning. Both halves are measured numbers, not a
// convention, so the schema has to be general enough to say them exactly -
// which is why a window carries a duration as well as an end time. An overnight
// is natural as an end; a weekend-long span is not expressible as one at all.
func TestMeasuredOffHoursWindow(t *testing.T) {
	scheds := compile(t, `
defaultLocation: Europe/London
schedules:
  - name: offhours
    type: intercept
    workload: checkout-api
    namespace: shop
    port: "8443"
    targetService: auth-stub.previews
    targetPort: 8443
    windows:
      - days: [Mon, Tue, Wed, Thu]
        start: "18:32"
        end:   "07:21"
      - days: [Fri]
        start: "18:32"
        duration: 60h49m
`)
	if len(scheds) != 1 {
		t.Fatalf("want 1 schedule, got %d", len(scheds))
	}
	sc := scheds[0]
	loc := london(t)

	at := func(s string) time.Time {
		t.Helper()
		ts, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}

	// 2026-09-14 is a Monday.
	cases := []struct {
		when string
		want bool
		why  string
	}{
		{"2026-09-14 09:00", false, "Monday morning, everything up"},
		{"2026-09-14 18:31", false, "one minute before the measured stop"},
		{"2026-09-14 18:32", true, "the measured stop, to the minute"},
		{"2026-09-15 03:00", true, "the middle of the night nobody is watching"},
		{"2026-09-15 07:20", true, "the last minute before the measured start"},
		{"2026-09-15 07:21", false, "the measured start: dev is back, stand down"},
		{"2026-09-15 12:00", false, "the middle of a working Tuesday"},
		// The brief's assumed 06:00 would have stood the intercept down here
		// and left dev broken for the first 80 minutes of every working day.
		{"2026-09-16 06:30", true, "still off-hours: 06:00 would have been wrong by 80 minutes"},
		// Friday evening through to Monday morning, continuously.
		{"2026-09-18 18:32", true, "Friday, the long window opens"},
		{"2026-09-19 14:00", true, "Saturday afternoon"},
		{"2026-09-20 14:00", true, "Sunday afternoon"},
		{"2026-09-21 07:20", true, "Monday, the last minute of the weekend window"},
		{"2026-09-21 07:21", false, "Monday 07:21: 60h49m from Friday 18:32, to the minute"},
		{"2026-09-21 09:00", false, "Monday morning, working again"},
	}
	for _, c := range cases {
		if got := sc.active(at(c.when)); got != c.want {
			t.Errorf("active(%s) = %v, want %v (%s)", c.when, got, c.want, c.why)
		}
	}
}

// TestWindowEdges covers the arithmetic the measured window does not reach: a
// window that does not wrap, one written as a whole day, and the day list
// aliases.
func TestWindowEdges(t *testing.T) {
	scheds := compile(t, `
schedules:
  - name: daytime
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows:
      - days: [weekdays]
        start: "09:00"
        end:   "17:00"
  - name: allweekend
    type: intercept
    workload: pricing
    namespace: shop
    targetService: stub
    windows:
      - days: [weekends]
        start: "00:00"
        end:   "24:00"
`)
	at := func(s string) time.Time {
		t.Helper()
		ts, err := time.ParseInLocation("2006-01-02 15:04", s, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}

	var daytime, allweekend *schedule
	for _, sc := range scheds {
		switch sc.spec.Name {
		case "daytime":
			daytime = sc
		case "allweekend":
			allweekend = sc
		}
	}

	// A non-wrapping window closes the same day and does not leak into the next.
	for _, c := range []struct {
		when string
		want bool
	}{
		{"2026-09-14 08:59", false},
		{"2026-09-14 09:00", true},
		{"2026-09-14 16:59", true},
		{"2026-09-14 17:00", false},
		{"2026-09-14 23:00", false},
		{"2026-09-19 12:00", false}, // Saturday is not a weekday
	} {
		if got := daytime.active(at(c.when)); got != c.want {
			t.Errorf("daytime.active(%s) = %v, want %v", c.when, got, c.want)
		}
	}

	// 00:00 to 24:00 is the whole day and nothing either side of it.
	for _, c := range []struct {
		when string
		want bool
	}{
		{"2026-09-18 23:59", false}, // Friday
		{"2026-09-19 00:00", true},  // Saturday, the first minute
		{"2026-09-20 23:59", true},  // Sunday, the last minute
		{"2026-09-21 00:00", false}, // Monday
	} {
		if got := allweekend.active(at(c.when)); got != c.want {
			t.Errorf("allweekend.active(%s) = %v, want %v", c.when, got, c.want)
		}
	}
}

// TestScheduleRefusals is the executable form of what a schedule may not say.
// Every one of these is fatal at startup rather than at 18:32, because the whole
// premise of this mode is that nobody is watching when the window opens.
func TestScheduleRefusals(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{{
		name: "REFUSED: intercept namespace off the allow-list",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: coredns
    namespace: kube-system
    targetService: stub.shop
    windows: [{days: [daily], start: "00:00", end: "24:00"}]`,
		wantErr: `namespace "kube-system" is not in`,
	}, {
		// The same boundary a preview's forward target is held to, and for the
		// same reason: a schedule's target is a plain net.Dial to a ClusterIP.
		name: "REFUSED: forward target off the allow-list",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: anything.kube-system
    windows: [{days: [daily], start: "00:00", end: "24:00"}]`,
		wantErr: `forward-target namespace "kube-system" is not in`,
	}, {
		name: "REFUSED: two schedules on one workload, which telepresence will not serve",
		yaml: `
schedules:
  - name: one
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]
  - name: two
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Tue], start: "18:00", end: "19:00"}]`,
		wantErr: "telepresence serves one global intercept per workload",
	}, {
		name: "REFUSED: two schedules of one name",
		yaml: `
schedules:
  - name: dup
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]
  - name: dup
    type: intercept
    workload: pricing
    namespace: shop
    targetService: stub
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: `both named "dup"`,
	}, {
		name: "REFUSED: no target to divert to",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: checkout-api
    namespace: shop
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "targetService is required",
	}, {
		name: "REFUSED: no window",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: []`,
		wantErr: "at least one window is required",
	}, {
		name: "REFUSED: both an end and a duration, which disagree by construction",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Mon], start: "18:00", end: "19:00", duration: 2h}]`,
		wantErr: "end or duration, not both",
	}, {
		name: "REFUSED: neither an end nor a duration",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Mon], start: "18:00"}]`,
		wantErr: "one of end or duration is required",
	}, {
		name: "REFUSED: a day that is not a day",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Blursday], start: "18:00", end: "19:00"}]`,
		wantErr: `day "Blursday" is not a day of the week`,
	}, {
		name: "REFUSED: a time that is not a time",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Mon], start: "half six", end: "19:00"}]`,
		wantErr: "is not HH:MM",
	}, {
		name: "REFUSED: a zone that does not exist",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    location: Mars/Olympus_Mons
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "Mars/Olympus_Mons",
	}, {
		name: "REFUSED: an empty file, which would be a mode that silently does nothing",
		yaml: `
schedules: []`,
		wantErr: "declares no schedules",
	}, {
		// The type is DECLARED, never inferred from which fields are filled
		// in. There is more than one kind now, so an entry that does not say
		// which it is is a question, and guessing the older kind is how a DNS
		// schedule written without a type gets read as an intercept with no
		// workload rather than as the thing it is.
		name: "REFUSED: no type at all",
		yaml: `
schedules:
  - name: s
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "type is required",
	}, {
		name: "REFUSED: a type that is not a kind of schedule",
		yaml: `
schedules:
  - name: s
    type: sql
    hostname: dev-sql.example.internal
    redirectTo: 10.42.0.9
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: `type "sql" is not a kind of schedule`,
	}, {
		// The mixed shapes, in both directions. An entry carrying both was
		// written by somebody who believed it would do both, and whichever
		// half won silently would be the one they were not thinking about.
		name: "REFUSED: an intercept schedule carrying DNS-mode fields",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    hostname: dev-sql.example.internal
    redirectTo: 10.42.0.9
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "carries the DNS-mode field(s) hostname/redirectTo",
	}, {
		name: "REFUSED: a DNS schedule carrying intercept-mode fields",
		yaml: `
schedules:
  - name: s
    type: dns
    hostname: dev-sql.example.internal
    redirectTo: 10.42.0.9
    workload: checkout-api
    namespace: shop
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "carries the intercept-mode field(s) workload/namespace",
	}, {
		name: "REFUSED: a DNS schedule with neither of its own fields",
		yaml: `
schedules:
  - name: s
    type: dns
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "hostname is required",
	}, {
		name: "REFUSED: a DNS schedule with a hostname and nowhere to point it",
		yaml: `
schedules:
  - name: s
    type: dns
    hostname: dev-sql.example.internal
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "redirectTo is required",
	}, {
		// redirectTo is a literal address on purpose: a Service name would put
		// a Kubernetes lookup, and a way for it to fail at 18:32 with nobody
		// watching, on the path of the one operation that must not be fragile.
		name: "REFUSED: redirectTo as a name rather than an address",
		yaml: `
schedules:
  - name: s
    type: dns
    hostname: dev-sql.example.internal
    redirectTo: sql-stub.previews.svc.cluster.local
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "must be a literal IP address, not a name",
	}, {
		name: "REFUSED: a hostname that is not a DNS name",
		yaml: `
schedules:
  - name: s
    type: dns
    hostname: "not a hostname"
    redirectTo: 10.42.0.9
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "is not a DNS name",
	}, {
		// The DNS kind's counterpart of "two schedules on one workload": two
		// hosts lines for one name resolve to whichever CoreDNS read first, so
		// the second one is silently nothing.
		name: "REFUSED: two schedules overriding one hostname",
		yaml: `
schedules:
  - name: one
    type: dns
    hostname: dev-sql.example.internal
    redirectTo: 10.42.0.9
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]
  - name: two
    type: dns
    hostname: dev-sql.example.internal
    redirectTo: 10.42.0.10
    windows: [{days: [Tue], start: "18:00", end: "19:00"}]`,
		wantErr: "both override the hostname dev-sql.example.internal",
	}, {
		name: "REFUSED: a typo in a field name, rather than ignoring it",
		yaml: `
schedules:
  - name: s
    type: intercept
    workload: checkout-api
    namespace: shop
    targetServices: stub
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "unknown field",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadSchedules(writeTemp(t, c.yaml), cfgFor("shop", "previews"))
			if err == nil {
				t.Fatalf("want an error containing %q, got none", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want an error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

// TestGlobalInterceptHasNoHeaderFilters is the one-line fact the whole mode
// rests on. The traffic-agent's test for HTTP mode is len(HeaderFilters) > 0, so
// a global intercept must carry none - not a filter with an empty value, which
// is still HTTP mode matching a blank header.
func TestGlobalInterceptHasNoHeaderFilters(t *testing.T) {
	sc := compile(t, `
schedules:
  - name: offhours
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [daily], start: "00:00", end: "24:00"}]
`)[0]

	p := sc.preview()
	if !p.Global {
		t.Fatal("a schedule's preview must be Global")
	}
	if f := p.headerFilters(); f != nil {
		t.Fatalf("a global intercept must carry NO header filters, got %v", f)
	}

	// And the header-keyed path is untouched: it still carries exactly one.
	hp, err := (&PreviewRequest{WorkID: "1234", Workload: "checkout-api",
		Namespace: "shop", PreviewService: "stub"}).validate(cfgFor("shop"))
	if err != nil {
		t.Fatal(err)
	}
	if f := hp.headerFilters(); len(f) != 1 || f["x-preview"] != "1234" {
		t.Fatalf("a header-keyed preview must still carry x-preview: 1234, got %v", f)
	}
	if hp.Global {
		t.Fatal("nothing in the HTTP API may produce a global intercept")
	}
}

// TestModeConflictIsRefusedBothWays is acceptance criterion 2. The failure it
// prevents is SILENT: a workload runs in one intercept mode at a time, and
// whichever mode loses is created, reported healthy, and matches nothing. So
// both directions have to be caught before the second intercept is raised.
func TestModeConflictIsRefusedBothWays(t *testing.T) {
	cfg := cfgFor("shop")
	sc := compile(t, `
schedules:
  - name: offhours
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [daily], start: "00:00", end: "24:00"}]
`)[0]
	global := sc.preview()

	header, err := (&PreviewRequest{WorkID: "1234", Workload: "checkout-api",
		Namespace: "shop", PreviewService: "checkout-api-preview"}).validate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	other, err := (&PreviewRequest{WorkID: "5678", Workload: "pricing",
		Namespace: "shop", PreviewService: "pricing-preview"}).validate(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Scheduled first, then a header-keyed preview on the same workload.
	reg := newRegistry()
	if err := reg.add(global); err != nil {
		t.Fatal(err)
	}
	if c := reg.conflictingMode(header); c == nil {
		t.Error("a header-keyed preview on a workload holding a global intercept must be refused")
	} else if c.Name != global.Name {
		t.Errorf("refused against %s, want %s", c.Name, global.Name)
	}

	// Header-keyed first, then the schedule's window opens on it.
	reg = newRegistry()
	if err := reg.add(header); err != nil {
		t.Fatal(err)
	}
	if c := reg.conflictingMode(global); c == nil {
		t.Error("a global intercept on a workload holding a header-keyed preview must be refused")
	} else if c.Name != header.Name {
		t.Errorf("refused against %s, want %s", c.Name, header.Name)
	}

	// And nothing else is affected: a preview of a DIFFERENT workload, and a
	// second header-keyed preview of the SAME workload, both still go up. Two
	// work ids on one service is the existing behaviour and this must not have
	// narrowed it.
	if c := reg.conflictingMode(other); c != nil {
		t.Errorf("a preview of a different workload was refused against %s", c.Name)
	}
	second, err := (&PreviewRequest{WorkID: "9999", Workload: "checkout-api",
		Namespace: "shop", PreviewService: "another-preview"}).validate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c := reg.conflictingMode(second); c != nil {
		t.Errorf("a second header-keyed preview of the same workload was refused against %s", c.Name)
	}
}

// TestScheduledInterceptIgnoresTheExpirySweep: a window that runs from Friday
// evening to Monday morning is longer than the default PREVIEW_LIFETIME, so an
// expiry on a scheduled intercept would tear it down mid-window. Its life is the
// window and nothing else.
func TestScheduledInterceptIgnoresTheExpirySweep(t *testing.T) {
	sc := compile(t, `
schedules:
  - name: offhours
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Fri], start: "18:32", duration: 60h49m}]
`)[0]
	reg := newRegistry()
	if err := reg.add(sc.preview()); err != nil {
		t.Fatal(err)
	}
	reg.touch("offhours", time.Now().Add(-time.Hour))
	if got := reg.expired(time.Now()); len(got) != 0 {
		t.Fatalf("a scheduled intercept must never be swept by expiry, got %v", got)
	}
	if p, _ := reg.get(sc.key()); !p.ExpiresAt.IsZero() {
		t.Fatalf("a scheduled intercept must carry no expiry, got %s", p.ExpiresAt)
	}
}

// TestNothingScheduledChangesNothing is acceptance criterion 4 at the config
// level: with no SCHEDULE_FILE there is no schedule, no reserved name, and the
// controller has nothing to do.
func TestNothingScheduledChangesNothing(t *testing.T) {
	cfg := cfgFor("shop")
	if len(cfg.schedules) != 0 {
		t.Fatal("a config with no schedule file must have no schedules")
	}
	if len(cfg.scheduledNames()) != 0 {
		t.Fatal("a config with no schedule file must reserve no work ids")
	}
}

// TestNextChange is what the startup log and GET /schedules report, and the one
// thing that tells an operator at 09:00 that the window they wrote means what
// they meant.
func TestNextChange(t *testing.T) {
	sc := compile(t, `
defaultLocation: Europe/London
schedules:
  - name: offhours
    type: intercept
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Mon, Tue, Wed, Thu], start: "18:32", end: "07:21"}]
`)[0]
	loc := london(t)
	from := time.Date(2026, 9, 14, 9, 0, 0, 0, loc) // Monday morning
	when, ok := sc.nextChange(from)
	if !ok {
		t.Fatal("want a next change within a fortnight")
	}
	if want := time.Date(2026, 9, 14, 18, 32, 0, 0, loc); !when.Equal(want) {
		t.Fatalf("next change %s, want %s", when.In(loc), want)
	}
	// And from inside the window, the next change is the close.
	when, _ = sc.nextChange(time.Date(2026, 9, 14, 23, 0, 0, 0, loc))
	if want := time.Date(2026, 9, 15, 7, 21, 0, 0, loc); !when.Equal(want) {
		t.Fatalf("next change %s, want %s", when.In(loc), want)
	}
}

// TestDNSScheduleSharesTheWindowMachinery is the DNS kind's schema test, and
// the point of it is what it does NOT have to prove again: the days, the
// overnight wrap, the duration and the zone are the same compiled window the
// intercept kind uses, because there is one implementation of them. What is
// new here is the target - a hostname and a literal address - and that the two
// kinds sit in one file without either knowing about the other.
func TestDNSScheduleSharesTheWindowMachinery(t *testing.T) {
	scheds := compile(t, `
defaultLocation: Europe/London
schedules:
  - name: offhours
    type: intercept
    workload: checkout-api
    namespace: shop
    port: "8443"
    targetService: auth-stub.previews
    targetPort: 8443
    windows:
      - days: [Mon, Tue, Wed, Thu]
        start: "18:32"
        end:   "07:21"
  - name: sql-offhours
    type: dns
    hostname: dev-sql.example.internal
    redirectTo: 10.42.0.9
    windows:
      - days: [Mon, Tue, Wed, Thu]
        start: "18:32"
        end:   "07:21"
      - days: [Fri]
        start: "18:32"
        duration: 60h49m
`)
	if len(scheds) != 2 {
		t.Fatalf("want 2 schedules, got %d", len(scheds))
	}
	// loadSchedules sorts by name, so the DNS one is second.
	intercept, dns := scheds[0], scheds[1]
	if intercept.kind != kindIntercept || dns.kind != kindDNS {
		t.Fatalf("kinds are %s and %s", intercept.kind, dns.kind)
	}
	if dns.hostname != "dev-sql.example.internal" || dns.redirectTo != "10.42.0.9" {
		t.Fatalf("DNS target compiled as %s -> %s", dns.hostname, dns.redirectTo)
	}
	// A DNS schedule raises no intercept, so it has no manager-side name and no
	// forward target - reporting empty ones would invite somebody to go looking
	// for an intercept that does not exist.
	if dns.interceptName != "" || dns.targetService != "" || dns.targetPort != 0 {
		t.Fatalf("a DNS schedule carries intercept fields: %+v", dns)
	}

	loc := london(t)
	when := func(s string) time.Time {
		t.Helper()
		ts, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	// Same window text, same answers, in both kinds - which is the whole claim
	// of reusing the machinery rather than writing a second scheduler.
	for _, c := range []struct {
		at   string
		want bool
	}{
		{"2026-09-14 18:31", false},
		{"2026-09-14 18:32", true},
		{"2026-09-15 07:21", false},
	} {
		if got := dns.active(when(c.at)); got != c.want {
			t.Errorf("dns.active(%s) = %v, want %v", c.at, got, c.want)
		}
		if got := intercept.active(when(c.at)); got != c.want {
			t.Errorf("intercept.active(%s) = %v, want %v", c.at, got, c.want)
		}
	}
	// The weekend-long window is the DNS one's alone, and it still works.
	if !dns.active(when("2026-09-19 14:00")) {
		t.Error("the Friday-evening window does not cover Saturday afternoon")
	}
	// A trailing dot on a hostname is the same name; it is normalised rather
	// than written into the hosts line as a second spelling of it.
	if got := compile(t, `
schedules:
  - name: dotted
    type: dns
    hostname: dev-sql.example.internal.
    redirectTo: 10.42.0.9
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]
`)[0].hostname; got != "dev-sql.example.internal" {
		t.Errorf("trailing dot not normalised: %q", got)
	}
}
