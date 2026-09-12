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
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows:
      - days: [weekdays]
        start: "09:00"
        end:   "17:00"
  - name: allweekend
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
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]
  - name: two
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
    workload: checkout-api
    namespace: shop
    targetService: stub
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]
  - name: dup
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
    workload: checkout-api
    namespace: shop
    windows: [{days: [Mon], start: "18:00", end: "19:00"}]`,
		wantErr: "targetService is required",
	}, {
		name: "REFUSED: no window",
		yaml: `
schedules:
  - name: s
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
		name: "REFUSED: a typo in a field name, rather than ignoring it",
		yaml: `
schedules:
  - name: s
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
