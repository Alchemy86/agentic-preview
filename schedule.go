package main

import (
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	// The image is distroless/static: it carries no /usr/share/zoneinfo, so
	// time.LoadLocation would fail for every name but UTC and Local. Embedding
	// the database is what makes "Europe/London" in a schedule mean what it
	// says - including the two DST shifts an overnight window sits across.
	_ "time/tzdata"

	"sigs.k8s.io/yaml"
)

// Scheduled, headerless intercepts.
//
// Everything else in this service is HEADER-KEYED: an intercept carries
// HeaderFilters, the traffic-agent therefore runs its port in HTTP mode, and
// only requests carrying that header are diverted. A scheduled intercept is the
// other shape telepresence offers and the one it calls GLOBAL: no filters at
// all, so the agent keeps the port on its raw TCP listener and io.Copy's the
// WHOLE connection to us. It never parses a byte, so it works identically for
// TLS and for plaintext.
//
// Three facts about that shape drive everything below.
//
//  1. THE TWO MODES DO NOT COMPOSE ON ONE WORKLOAD. "Has filters" is a
//     per-workload switch in the agent (cmd/traffic/cmd/agent/fwd/tcp.go:
//     setListenerSwitch, from interceptController.isHTTP). If ANY
//     header-filtered intercept is live on the workload the port flips to HTTP
//     mode, and the matcher there has exactly two tiers - header, then path. An
//     intercept with neither qualifies for neither and falls through to the
//     real application. It is not a low-priority default tier; it is INERT, and
//     silently so. So a scheduled intercept refuses to open on a workload that
//     already carries a header-keyed preview, and a header-keyed preview is
//     refused on a workload carrying a live scheduled one. See
//     registry.conflictingMode - that refusal is the whole reason it exists.
//
//  2. AN ORPHANED GLOBAL INTERCEPT HANGS ALL OF THE WORKLOAD'S TRAFFIC, not one
//     header's worth. docs/LIMITS.md has the measurement: on a forced kill the
//     intercept stays in manager state with nobody holding its tunnel, and the
//     agent's fail-open branch covers a BROKEN client stream, not an ABSENT
//     one, so requests hang rather than falling through. For a header-keyed
//     preview that costs one header until PREVIEW_LIFETIME sweeps it. For a
//     global one it is the entire service, and nobody is awake at 03:00 to
//     notice. That is why the controller below re-checks its own intercept
//     against the manager every SCHEDULE_CHECK_INTERVAL (30s) instead of
//     trusting the expiry sweep, and why SweepPreviousSession already departing
//     the dead session at startup matters far more in this mode.
//
//  3. NOTHING WITHOUT A DECLARED WINDOW IS AFFECTED. No schedule file, no
//     behaviour change: RunSchedules returns immediately and not one line of the
//     header-keyed path is reached.
//
// TWO KINDS OF SCHEDULE SHARE ALL OF THAT. A schedule is either an INTERCEPT
// schedule - everything above - or a DNS-REDIRECT one, and the windows, the
// time zone handling, the reconcile loop, the drift check and the alarm are the
// same machinery for both. The second kind exists because the first can only
// reach a Kubernetes workload: an intercept needs a pod with a traffic-agent on
// it, so a dependency that is not a workload at all - a managed database behind
// Private Link, an appliance, anything with a DNS name and no Deployment - has
// no interception point in the cluster except the resolver. A DNS-redirect
// schedule takes that point: while its window is open it writes one `hosts`
// line into a CoreDNS-style ConfigMap so the name resolves somewhere else, and
// removes exactly that line when the window closes. See dns.go.

// scheduleConfig is the whole file named by SCHEDULE_FILE, YAML or JSON.
type scheduleConfig struct {
	// DefaultLocation is the IANA time zone every window is read in unless it
	// names its own. Defaults to UTC, which is stated rather than guessed: a
	// window given in local time and read in UTC is silently an hour wrong for
	// half the year, which is the worst way for this to fail.
	DefaultLocation string `json:"defaultLocation,omitempty"`

	Schedules []ScheduleSpec `json:"schedules"`
}

// scheduleKind is which of the two shapes a ScheduleSpec is. An entry is
// exactly one of them and saying so is not optional: the two carry disjoint
// fields, and an entry with fields from both is a config whose author believed
// something untrue about what it would do.
type scheduleKind int

const (
	kindIntercept scheduleKind = iota
	kindDNS
)

func (k scheduleKind) String() string {
	if k == kindDNS {
		return "dns"
	}
	return "intercept"
}

// ScheduleSpec declares one target that is diverted for a recurring window and
// left completely alone outside it.
//
// It is exactly ONE of two kinds, and the entry SAYS WHICH in `type`:
//
//   - type: intercept - workload + namespace + targetService. One Kubernetes
//     workload is globally intercepted while the window is open.
//   - type: dns - hostname + redirectTo. One DNS name resolves somewhere else
//     while the window is open, by way of a line in a CoreDNS-style ConfigMap.
//
// The type is declared rather than inferred from which fields are filled in,
// and it is required. Inference reads an entry's fields and decides what its
// author must have meant; a declaration lets the two be CHECKED against each
// other, so an entry that says `intercept` and carries a `hostname` is a
// refusal at boot instead of a hostname that silently does nothing.
//
// Name, Location and Windows are shared and mean the same thing in both kinds.
type ScheduleSpec struct {
	// Name identifies the schedule. For an intercept schedule it is also the
	// work id the intercept is registered and reported under, so it must be
	// shaped like one, and it is RESERVED: a POST /previews naming it is
	// refused rather than allowed to share a registry entry with the schedule.
	Name string `json:"name"`

	// Type is which kind of schedule this is: "intercept" or "dns". Required.
	Type string `json:"type"`

	// Workload and Namespace name the live workload intercepted.
	Workload  string `json:"workload"`
	Namespace string `json:"namespace"`

	// Port is the port identifier on the intercepted workload (a service port
	// name or number). Defaults to 80.
	Port string `json:"port,omitempty"`

	// Service disambiguates which Service is intercepted, for a workload that
	// has more than one on the port. Without it the manager refuses with
	// "multiple interceptable services with port N - please specify the
	// service", which reads like a CLI flag this service does not have.
	Service string `json:"service,omitempty"`

	// TargetService is the Service traffic is diverted to, as "name" or
	// "name.namespace". Resolved to a literal ClusterIP before the intercept is
	// created, exactly as a preview's target is.
	TargetService   string `json:"targetService"`
	TargetNamespace string `json:"targetNamespace,omitempty"`

	// TargetPort is the port on TargetService. Defaults to 80.
	TargetPort int `json:"targetPort,omitempty"`

	// Location is the IANA time zone this schedule's windows are read in,
	// overriding the file's defaultLocation.
	Location string `json:"location,omitempty"`

	// Hostname is the DNS name overridden while the window is open. Setting it
	// makes this a DNS-redirect schedule, and every intercept-mode field above
	// must then be empty.
	Hostname string `json:"hostname,omitempty"`

	// RedirectTo is the address Hostname resolves to while the window is open.
	// It is a LITERAL IP, not a Service name.
	//
	// A Service name would have to be resolved to a ClusterIP at open time,
	// which puts a Kubernetes lookup - and a way for it to fail, at 18:32, with
	// nobody watching - on the path of the one operation that must not be
	// fragile. A literal address is proved parseable at boot, which is the same
	// posture as every other field here. Point it at a Service's ClusterIP by
	// writing that ClusterIP down: a ClusterIP is stable for the life of the
	// Service, and a Service recreated with a new one is a change worth having
	// to make deliberately.
	RedirectTo string `json:"redirectTo,omitempty"`

	// Windows are the recurring spans the schedule is up for. Several are
	// allowed and they may overlap; it is up if any of them covers the moment.
	Windows []Window `json:"windows"`
}

// Window is one recurring span: it OPENS at Start on each of Days and runs for
// Duration, or until End.
//
// Days are the days the window STARTS on, never the days it covers - a window
// that opens on Friday evening and closes on Monday morning is one window with
// days: [Fri], not three. That is the whole reason Duration exists alongside
// End: an 18:32-07:21 overnight is natural to write as an end time, and a
// weekend-long span is not expressible as one at all.
type Window struct {
	// Days the window opens on: Mon..Sun in any case, long or short, plus the
	// aliases "weekdays", "weekends" and "daily".
	Days []string `json:"days"`

	// Start is the opening wall-clock time, "HH:MM" or "HH:MM:SS".
	Start string `json:"start"`

	// End is the closing wall-clock time. An End at or before Start means the
	// next day, so "18:32" to "07:21" is the overnight it reads as. Exactly one
	// of End and Duration is required.
	End string `json:"end,omitempty"`

	// Duration is how long the window stays open, as a Go duration ("12h49m").
	// Use it for anything longer than a day: "60h49m" from Fri 18:32 is Monday
	// 07:21.
	Duration string `json:"duration,omitempty"`
}

// schedule is a compiled ScheduleSpec: everything validated once at startup so
// that the controller loop can only succeed or find the manager unwilling.
type schedule struct {
	spec ScheduleSpec

	// kind is which of the two shapes this is. Every field below it is
	// meaningful for one kind and zero for the other.
	kind scheduleKind

	// interceptName is the manager-side intercept name.
	interceptName string

	targetService   string
	targetNamespace string
	targetPort      int
	portID          string

	// hostname and redirectTo are the DNS kind's whole target: the name that is
	// overridden and the address it points at while the window is open.
	hostname   string
	redirectTo string

	loc     *time.Location
	windows []window
}

// window is a compiled Window: start is the offset from local midnight and
// length is how long it stays open from there.
type window struct {
	days   [7]bool
	start  time.Duration
	length time.Duration
}

func (s *schedule) key() serviceKey {
	return serviceKey{WorkID: s.spec.Name, Namespace: s.spec.Namespace, Workload: s.spec.Workload}
}

func (s *schedule) target() string { return s.targetService + "." + s.targetNamespace }

// preview is the desired state a schedule reconciles to while its window is
// open. It goes in the same registry as every header-keyed preview, which is
// not a shortcut: the registry is what re-raises everything onto a rebuilt
// manager session, and what Shutdown walks to remove every intercept before
// departing. A scheduled intercept needs both of those more than a preview
// does, so it gets them by being in the same place.
func (s *schedule) preview() *Preview {
	return &Preview{
		WorkID:        s.spec.Name,
		Workload:      s.spec.Workload,
		Namespace:     s.spec.Namespace,
		Name:          s.interceptName,
		Global:        true,
		Schedule:      s.spec.Name,
		PortID:        s.portID,
		ServiceName:   s.spec.Service,
		TargetService: s.target(),
		TargetPort:    s.targetPort,
		Disposition:   "PENDING",
	}
}

// active reports whether any of the schedule's windows covers t.
//
// A window that is longer than a day may have opened several days before t, so
// each candidate opening day back to the window's own length is considered. The
// opening instant is built as local midnight plus the offset rather than by
// arithmetic on t, so an opening survives a DST shift as the wall-clock time it
// was written as.
func (s *schedule) active(t time.Time) bool {
	lt := t.In(s.loc)
	for _, w := range s.windows {
		back := int(w.length/(24*time.Hour)) + 1
		for d := 0; d <= back; d++ {
			day := lt.AddDate(0, 0, -d)
			if !w.days[int(day.Weekday())] {
				continue
			}
			open := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, s.loc).Add(w.start)
			if !lt.Before(open) && lt.Before(open.Add(w.length)) {
				return true
			}
		}
	}
	return false
}

// nextChange returns when active() next flips, searched at minute resolution
// over the coming fortnight. It exists for the status API and the startup log:
// "the window opens in 4h12m" is the one line that tells an operator the
// schedule they wrote means what they meant, without waiting for 18:32.
func (s *schedule) nextChange(from time.Time) (time.Time, bool) {
	now := s.active(from)
	t := from.Truncate(time.Minute)
	for i := 0; i < 14*24*60; i++ {
		t = t.Add(time.Minute)
		if s.active(t) != now {
			return t, true
		}
	}
	return time.Time{}, false
}

// describe is the schedule in one line, for the startup log.
func (s *schedule) describe() string {
	parts := make([]string, 0, len(s.windows))
	for _, w := range s.windows {
		parts = append(parts, fmt.Sprintf("%s %s for %s", w.dayNames(), clock(w.start), w.length))
	}
	if s.kind == kindDNS {
		return fmt.Sprintf("%s: DNS %s -> %s, %s (%s)",
			s.spec.Name, s.hostname, s.redirectTo, strings.Join(parts, "; "), s.loc)
	}
	return fmt.Sprintf("%s: %s.%s port %s -> %s:%d, %s (%s)",
		s.spec.Name, s.spec.Workload, s.spec.Namespace, s.portID,
		s.target(), s.targetPort, strings.Join(parts, "; "), s.loc)
}

func (w window) dayNames() string {
	var out []string
	for i := range w.days {
		if w.days[i] {
			out = append(out, time.Weekday(i).String()[:3])
		}
	}
	return strings.Join(out, ",")
}

func clock(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d/time.Hour), int(d/time.Minute)%60)
}

// loadSchedules reads and compiles SCHEDULE_FILE. An unset path is the normal
// case and yields nothing; a path that is set and unreadable, or a file that
// does not compile, is FATAL. A schedule silently not running is the failure
// this mode cannot afford: the whole point is that nobody is watching.
func loadSchedules(path string, cfg *config) ([]*schedule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading SCHEDULE_FILE %s: %w", path, err)
	}
	var sc scheduleConfig
	if err := yaml.UnmarshalStrict(b, &sc); err != nil {
		return nil, fmt.Errorf("parsing SCHEDULE_FILE %s: %w", path, err)
	}
	if len(sc.Schedules) == 0 {
		return nil, fmt.Errorf("SCHEDULE_FILE %s declares no schedules: unset the variable instead of shipping an empty file", path)
	}

	defLoc := strings.TrimSpace(sc.DefaultLocation)
	if defLoc == "" {
		defLoc = "UTC"
	}

	out := make([]*schedule, 0, len(sc.Schedules))
	byName := map[string]bool{}
	byWorkload := map[string]string{}
	byHost := map[string]string{}
	for i := range sc.Schedules {
		s, err := compileSchedule(sc.Schedules[i], defLoc, cfg)
		if err != nil {
			return nil, fmt.Errorf("schedule %d (%q): %w", i, sc.Schedules[i].Name, err)
		}
		if byName[s.spec.Name] {
			return nil, fmt.Errorf("two schedules are both named %q", s.spec.Name)
		}
		byName[s.spec.Name] = true
		switch s.kind {
		case kindDNS:
			// Two schedules overriding one name would write two hosts lines
			// for it and CoreDNS would answer with whichever it read first.
			// The same class of refusal as the workload one below, and for the
			// same reason: the second one silently does nothing.
			if other, dup := byHost[s.hostname]; dup {
				return nil, fmt.Errorf("schedules %q and %q both override the hostname %s: "+
					"two hosts lines for one name resolve to whichever CoreDNS reads first, "+
					"so the second would never be the answer",
					other, s.spec.Name, s.hostname)
			}
			byHost[s.hostname] = s.spec.Name
		default:
			// Two global intercepts on one workload is not a thing telepresence
			// will serve: interceptControllerMap.global() refuses with "multiple
			// intercepts found when requesting the global intercept" and the
			// second one is dead weight. Refuse it here, where it is readable,
			// rather than at 18:32 in a log nobody is reading.
			wk := s.spec.Workload + "." + s.spec.Namespace
			if other, dup := byWorkload[wk]; dup {
				return nil, fmt.Errorf("schedules %q and %q both intercept %s: "+
					"telepresence serves one global intercept per workload, so the second would never match",
					other, s.spec.Name, wk)
			}
			byWorkload[wk] = s.spec.Name
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].spec.Name < out[j].spec.Name })
	return out, nil
}

// compileSchedule validates one entry and compiles it. The shared half - the
// name, the time zone and the windows - is the same for both kinds by
// construction: it is done here, once, before either kind's own half runs.
func compileSchedule(spec ScheduleSpec, defLoc string, cfg *config) (*schedule, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return nil, fmt.Errorf("name is required: it is the id this schedule is reported under")
	}
	if !workIDRE.MatchString(spec.Name) {
		return nil, fmt.Errorf("name %q must be alphanumeric, optionally with dots, dashes or underscores", spec.Name)
	}

	kind, err := scheduleKindOf(spec)
	if err != nil {
		return nil, err
	}

	loc := strings.TrimSpace(spec.Location)
	if loc == "" {
		loc = defLoc
	}
	l, err := time.LoadLocation(loc)
	if err != nil {
		return nil, fmt.Errorf("location %q: %w", loc, err)
	}

	if len(spec.Windows) == 0 {
		return nil, fmt.Errorf("at least one window is required")
	}
	ws := make([]window, 0, len(spec.Windows))
	for i, w := range spec.Windows {
		cw, err := compileWindow(w)
		if err != nil {
			return nil, fmt.Errorf("window %d: %w", i, err)
		}
		ws = append(ws, cw)
	}

	sc := &schedule{spec: spec, kind: kind, loc: l, windows: ws}
	if kind == kindDNS {
		return sc, compileDNSSchedule(sc)
	}
	return sc, compileInterceptSchedule(sc, cfg)
}

// scheduleKindOf reads the declared type and checks the entry's fields against
// it.
//
// Both halves matter. The type is what the entry MEANS; the field check is what
// stops it meaning one thing and being written as another. An entry carrying
// both a workload and a hostname was written by somebody who believed it would
// do both, and whichever half won silently would be the one they were not
// thinking about - so it is fatal at boot, where somebody is looking, which is
// the posture the rest of this file already takes.
func scheduleKindOf(spec ScheduleSpec) (scheduleKind, error) {
	set := func(pairs ...[2]string) []string {
		var out []string
		for _, p := range pairs {
			if strings.TrimSpace(p[1]) != "" {
				out = append(out, p[0])
			}
		}
		return out
	}
	intercept := set(
		[2]string{"workload", spec.Workload},
		[2]string{"namespace", spec.Namespace},
		[2]string{"service", spec.Service},
		[2]string{"targetService", spec.TargetService},
		[2]string{"targetNamespace", spec.TargetNamespace},
		[2]string{"port", spec.Port},
	)
	if spec.TargetPort != 0 {
		intercept = append(intercept, "targetPort")
	}
	dns := set(
		[2]string{"hostname", spec.Hostname},
		[2]string{"redirectTo", spec.RedirectTo},
	)

	switch strings.ToLower(strings.TrimSpace(spec.Type)) {
	case "":
		// Deliberately not defaulted to "intercept". There is more than one
		// kind now, so an entry that does not say which it is is a question,
		// and answering it by assuming the older kind is how a DNS schedule
		// written without a type would be read as an intercept with no
		// workload rather than as the thing it is.
		return 0, fmt.Errorf("type is required, and must be %q or %q: it says which kind of schedule this is. "+
			"An existing schedule that intercepts a workload is `type: intercept`",
			kindIntercept, kindDNS)
	case kindIntercept.String():
		if len(dns) > 0 {
			return 0, fmt.Errorf("is `type: intercept` but carries the DNS-mode field(s) %s: "+
				"an entry is exactly one kind. Split it into two schedules, or change the type",
				strings.Join(dns, "/"))
		}
		return kindIntercept, nil
	case kindDNS.String():
		if len(intercept) > 0 {
			return 0, fmt.Errorf("is `type: dns` but carries the intercept-mode field(s) %s: "+
				"an entry is exactly one kind. Split it into two schedules, or change the type",
				strings.Join(intercept, "/"))
		}
		return kindDNS, nil
	default:
		return 0, fmt.Errorf("type %q is not a kind of schedule: it is %q (globally intercept a workload) "+
			"or %q (redirect a DNS name)", spec.Type, kindIntercept, kindDNS)
	}
}

// compileDNSSchedule fills in the DNS kind's half.
//
// Note what it does NOT check: ALLOWED_NAMESPACES. That fence bounds where an
// intercept may be raised and where a tunnel may be dialled, and a DNS redirect
// does neither - it writes one line into one ConfigMap named by
// SCHEDULE_DNS_CONFIGMAP, which is its own, separate fence and the only place
// this kind can write at all.
func compileDNSSchedule(sc *schedule) error {
	host := strings.TrimSpace(sc.spec.Hostname)
	if host == "" {
		return fmt.Errorf("hostname is required alongside redirectTo: the DNS name overridden while the window is open")
	}
	host = strings.TrimSuffix(host, ".")
	if !hostnameRE.MatchString(host) {
		return fmt.Errorf("hostname %q is not a DNS name", sc.spec.Hostname)
	}

	to := strings.TrimSpace(sc.spec.RedirectTo)
	if to == "" {
		return fmt.Errorf("redirectTo is required alongside hostname: the literal IP address %s resolves to "+
			"while the window is open", host)
	}
	addr, err := netip.ParseAddr(to)
	if err != nil {
		return fmt.Errorf("redirectTo %q must be a literal IP address, not a name: %w", sc.spec.RedirectTo, err)
	}
	if addr.Zone() != "" {
		return fmt.Errorf("redirectTo %q must not carry a zone", sc.spec.RedirectTo)
	}

	sc.hostname, sc.redirectTo = host, addr.Unmap().String()
	return nil
}

// compileInterceptSchedule fills in the intercept kind's half: the workload
// intercepted, the Service traffic is diverted to, and the two ports.
func compileInterceptSchedule(sc *schedule, cfg *config) error {
	spec := sc.spec
	spec.Workload = strings.TrimSpace(spec.Workload)
	spec.Namespace = strings.TrimSpace(spec.Namespace)
	if spec.Workload == "" || spec.Namespace == "" {
		return fmt.Errorf("workload and namespace are both required")
	}
	if !nameRE.MatchString(spec.Workload) {
		return fmt.Errorf("workload %q must be a DNS label", spec.Workload)
	}
	// The same boundary as a preview's, and for the same reason: this is the
	// only thing bounding where a schedule may intercept.
	if !cfg.namespaceAllowed(spec.Namespace) {
		return fmt.Errorf("namespace %q is not in agentic-preview's allowed set (%s)",
			spec.Namespace, strings.Join(cfg.allowedNamespaces, ", "))
	}
	if svc := strings.TrimSpace(spec.Service); svc != "" {
		if !nameRE.MatchString(svc) {
			return fmt.Errorf("service %q must be a DNS label", svc)
		}
		spec.Service = svc
	}

	svc := strings.TrimSpace(spec.TargetService)
	if svc == "" {
		return fmt.Errorf("targetService is required: the Service traffic is diverted to while the window is open")
	}
	svcNS := strings.TrimSpace(spec.TargetNamespace)
	if host, rest, ok := strings.Cut(svc, "."); ok {
		svc = host
		if svcNS == "" {
			svcNS, _, _ = strings.Cut(rest, ".")
		}
	}
	if svcNS == "" {
		svcNS = spec.Namespace
	}
	if !nameRE.MatchString(svc) {
		return fmt.Errorf("targetService %q must be a DNS label, optionally as name.namespace", spec.TargetService)
	}
	if !nameRE.MatchString(svcNS) {
		return fmt.Errorf("target namespace %q must be a DNS label", svcNS)
	}
	// ALLOWED_NAMESPACES bounds where traffic is forwarded TO as well as where
	// it is intercepted, and a schedule's target is a plain net.Dial to a
	// ClusterIP exactly as a preview's is. Same check, same words.
	if !cfg.namespaceAllowed(svcNS) {
		return fmt.Errorf("forward-target namespace %q is not in agentic-preview's allowed set (%s): "+
			"this is the namespace of targetService (what traffic is diverted TO), not %q, the intercepted workload's namespace",
			svcNS, strings.Join(cfg.allowedNamespaces, ", "), spec.Namespace)
	}

	port := spec.TargetPort
	if port == 0 {
		port = 80
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("targetPort %d is not a port", port)
	}
	portID := strings.TrimSpace(spec.Port)
	if portID == "" {
		portID = "80"
	}

	name := sanitise(spec.Workload + "-" + spec.Name)
	if !nameRE.MatchString(name) {
		return fmt.Errorf("workload %q and name %q produce an unusable intercept name %q",
			spec.Workload, spec.Name, name)
	}

	sc.spec = spec
	sc.interceptName = name
	sc.targetService, sc.targetNamespace = svc, svcNS
	sc.targetPort, sc.portID = port, portID
	return nil
}

func compileWindow(w Window) (window, error) {
	var out window
	if len(w.Days) == 0 {
		return out, fmt.Errorf("days is required")
	}
	for _, d := range w.Days {
		if err := setDays(&out.days, d); err != nil {
			return out, err
		}
	}

	start, err := parseClock(w.Start)
	if err != nil {
		return out, fmt.Errorf("start: %w", err)
	}
	if start >= 24*time.Hour {
		return out, fmt.Errorf("start %q must be before 24:00", w.Start)
	}
	out.start = start

	hasEnd := strings.TrimSpace(w.End) != ""
	hasDur := strings.TrimSpace(w.Duration) != ""
	switch {
	case hasEnd && hasDur:
		return out, fmt.Errorf("give end or duration, not both")
	case hasDur:
		d, err := time.ParseDuration(strings.TrimSpace(w.Duration))
		if err != nil {
			return out, fmt.Errorf("duration: %w", err)
		}
		if d <= 0 {
			return out, fmt.Errorf("duration %q must be positive", w.Duration)
		}
		out.length = d
	case hasEnd:
		end, err := parseClock(w.End)
		if err != nil {
			return out, fmt.Errorf("end: %w", err)
		}
		if end > 24*time.Hour {
			return out, fmt.Errorf("end %q must be at or before 24:00", w.End)
		}
		// An end at or before the start is the next day: "18:32" to "07:21" is
		// the overnight it reads as, and "00:00" to "00:00" is the whole day.
		if end <= start {
			end += 24 * time.Hour
		}
		out.length = end - start
	default:
		return out, fmt.Errorf("one of end or duration is required")
	}
	return out, nil
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

func setDays(into *[7]bool, name string) error {
	switch n := strings.ToLower(strings.TrimSpace(name)); n {
	case "daily", "every-day", "everyday", "all":
		for i := range into {
			into[i] = true
		}
	case "weekdays", "weekday":
		for _, d := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday} {
			into[int(d)] = true
		}
	case "weekends", "weekend":
		into[int(time.Saturday)] = true
		into[int(time.Sunday)] = true
	default:
		d, ok := dayNames[n]
		if !ok {
			return fmt.Errorf("day %q is not a day of the week (Mon..Sun) or one of weekdays, weekends, daily", name)
		}
		into[int(d)] = true
	}
	return nil
}

// parseClock reads "HH:MM" or "HH:MM:SS" as an offset from midnight. 24:00 is
// accepted so that a whole day can be written as 00:00 to 24:00.
func parseClock(s string) (time.Duration, error) {
	f := strings.Split(strings.TrimSpace(s), ":")
	if len(f) < 2 || len(f) > 3 {
		return 0, fmt.Errorf("%q is not HH:MM or HH:MM:SS", s)
	}
	units := []time.Duration{time.Hour, time.Minute, time.Second}
	limits := []int{24, 59, 59}
	var total time.Duration
	for i, part := range f {
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0, fmt.Errorf("%q is not HH:MM or HH:MM:SS", s)
		}
		if n < 0 || n > limits[i] {
			return 0, fmt.Errorf("%q is out of range", s)
		}
		total += time.Duration(n) * units[i]
	}
	if total > 24*time.Hour {
		return 0, fmt.Errorf("%q is past 24:00", s)
	}
	return total, nil
}
