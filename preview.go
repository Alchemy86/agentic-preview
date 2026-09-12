package main

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	workIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

// maxPreviewReplicas caps what one request may ask for. A preview is something
// to look at, not something to load-test against.
const maxPreviewReplicas = 10

// PreviewRequest adds ONE service to ONE work id.
//
// The unit is a service, never a repository: one namespace commonly holds a
// dozen services, and a PR that touches checkout must preview checkout and
// nothing else. The caller states the service; agentic-preview never infers a
// service set from a repo.
//
// Calling this repeatedly with the same workId ADDS to that id's set, which is
// how a change spanning several repos - the API, the worker, the shared client
// library, as separate PRs - ends up reachable behind a single header value as
// each PR builds.
type PreviewRequest struct {
	// WorkID is the work item the change belongs to: whatever id your issue
	// tracker gives one piece of work.
	// It is the header VALUE, so every service raised under it is reachable
	// with one header. It is deliberately not a PR number: one piece of work
	// has several PRs, and they must share a header.
	WorkID string `json:"workId"`

	// Workload and Namespace name the live workload being intercepted.
	Workload  string `json:"workload"`
	Namespace string `json:"namespace"`

	// PreviewService is the Service diverted traffic is forwarded to, as
	// "name" or "name.namespace". Resolved to a literal ClusterIP before the
	// intercept is created: the traffic-agent parses target_host with
	// iputil.ParseAddr and rejects a name.
	PreviewService   string `json:"previewService"`
	PreviewNamespace string `json:"previewNamespace,omitempty"`

	// PreviewPort is the port on PreviewService. Defaults to 80.
	PreviewPort int `json:"previewPort,omitempty"`

	// Port is the port identifier on the intercepted workload (a service port
	// name or number). Defaults to 80.
	Port string `json:"port,omitempty"`

	// ---- Creating the preview, rather than routing to one you deployed ----
	//
	// Set Image and agentic-preview builds the preview itself: it reads the
	// live Deployment named by Workload, copies it with that image in place of
	// the live one, and creates a Service in front of it. PreviewService is
	// then derived and must not be given.
	//
	// Leave it unset and nothing is created; PreviewService is required and
	// the service does the routing half only, as it always did.

	// Image is the image reference the preview runs, VERBATIM. Setting it is
	// what asks for a preview to be built.
	//
	// agentic-preview neither parses it nor completes it: no registry is
	// assumed, no repository is inherited from the live container, no tag
	// convention is understood. Whatever built and pushed that image knows
	// where it put it; this service does not, and a service that guessed would
	// only ever be right for one company's pipeline.
	Image string `json:"image,omitempty"`

	// Container names which container's image to swap. Only needed when the
	// live pod has several containers and none is named after the workload.
	Container string `json:"container,omitempty"`

	// Replicas is how many preview pods to run. Defaults to 1.
	Replicas int32 `json:"replicas,omitempty"`

	// SourceService is the live Service whose ports the preview Service
	// copies. Defaults to Workload, which is what it is called almost always.
	SourceService string `json:"sourceService,omitempty"`
}

// serviceKey identifies one service within one work id.
type serviceKey struct {
	WorkID    string
	Namespace string
	Workload  string
}

// Preview is one intercepted service under one work id: the desired state the
// service reconciles a manager session to. It carries everything needed to
// re-raise the intercept against a brand new session.
type Preview struct {
	WorkID    string `json:"workId"`
	Workload  string `json:"workload"`
	Namespace string `json:"namespace"`

	// Name is the manager-side intercept name, derived from workload and
	// work id so that two work ids previewing the same service coexist.
	Name string `json:"name"`

	HeaderName  string `json:"headerName,omitempty"`
	HeaderValue string `json:"headerValue,omitempty"`
	PortID      string `json:"port"`

	// Global makes this a HEADERLESS, whole-workload intercept: the spec goes
	// to the manager with no HeaderFilters at all, so the traffic-agent keeps
	// the port on its raw TCP listener and diverts every connection to it
	// rather than only those carrying a header. See the header comment on
	// schedule.go, which is where the consequences are written down - chiefly
	// that a workload runs in ONE mode at a time and the two silently exclude
	// each other, and that an orphaned global intercept hangs everything rather
	// than one header's worth.
	//
	// Nothing in the HTTP API sets it. It is set only by a declared schedule,
	// so a service with no window is untouched by every line it reaches.
	Global bool `json:"global,omitempty"`

	// Schedule is the name of the schedule that owns this intercept, empty for
	// every preview raised through /previews. It is what makes a scheduled
	// intercept immune to the expiry sweep and to a DELETE: its lifetime is the
	// window, and the controller is the only thing entitled to end it.
	Schedule string `json:"schedule,omitempty"`

	// ServiceName pins which Service is intercepted, for a workload carrying
	// more than one on the port. Left empty the manager resolves it, and
	// refuses when the choice is ambiguous.
	ServiceName string `json:"serviceName,omitempty"`

	TargetService string `json:"previewService"`
	TargetHost    string `json:"targetHost"`
	TargetPort    int    `json:"targetPort"`

	// Disposition is the manager's last reported state (WAITING, ACTIVE, ...),
	// or a local note before the intercept exists.
	Disposition string `json:"disposition"`
	Message     string `json:"message,omitempty"`

	// AgentPod is the node-agent pod currently carrying this workload's
	// tunnel; empty when no dial loop is established.
	AgentPod string `json:"agentPod,omitempty"`

	// Image is the image reference the preview is running, when agentic-preview
	// built the preview itself. Empty when the caller deployed the preview and
	// only asked for routing - nothing here reads a Service's pods to find out
	// what somebody else deployed.
	//
	// It is reported by GET /previews so that an adopter running an
	// image-retention policy can exclude images a live preview references. A
	// preview that outlives its image survives until its pod is replaced and
	// then cannot pull, which looks like a fault in this service and is not
	// one.
	Image string `json:"image,omitempty"`

	// RaisedAt is when this preview first appeared, and Age is the same thing
	// in words. They are reported ALWAYS, expiry or no expiry: with expiry
	// switched off, age is the only thing a person or a supervising process
	// has to notice previews accumulating.
	RaisedAt time.Time `json:"raisedAt,omitzero"`
	Age      string    `json:"age,omitempty"`

	// ExpiresAt is when this preview is swept if nothing touches it before
	// then. Reported so that an expiry is visible BEFORE it happens and
	// somebody can extend it rather than discover it went. Zero when expiry is
	// switched off, in which case nothing removes a preview but a DELETE.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`

	// Create is what agentic-preview was asked to BUILD for this preview, or
	// nil when the caller brought their own Service. It is the request's
	// desired state, kept so a re-POST reconciles onto the same objects.
	Create *CreateSpec `json:"-"`

	// Created is what it actually built. Nil when it built nothing, which is
	// also what makes a DELETE safe: it only ever removes objects carrying its
	// own labels.
	Created *CreatedObjects `json:"created,omitempty"`
}

func (p *Preview) key() serviceKey { return serviceKey{p.WorkID, p.Namespace, p.Workload} }

// agentKey is the identity a dial loop is kept under: one tunnel per agent pod
// per session, shared by every preview of that workload whatever its work id.
func (p *Preview) agentKey() string { return p.Workload + "." + p.Namespace }

// headerFilters is the InterceptSpec field that decides the whole mechanism.
//
// A nil return is what makes an intercept GLOBAL: the traffic-agent's test is
// len(spec.HeaderFilters) > 0 || len(spec.PathFilters) > 0
// (cmd/traffic/cmd/agent/fwd/interceptcontroller.go), and with neither the
// port's listener switch stays on the raw TCP side and the whole connection is
// io.Copy'd to us without a byte being parsed. A header filter whose VALUE is
// empty is not the same thing - that is still HTTP mode, matching a header that
// is present and blank - so this returns nil rather than an empty value.
func (p *Preview) headerFilters() map[string]string {
	if p.Global {
		return nil
	}
	return map[string]string{p.HeaderName: p.HeaderValue}
}

// Work is one work id's whole set, as the list API reports it, so a person can
// see that their change spans checkout and pricing and nothing else.
type Work struct {
	WorkID   string    `json:"workId"`
	Header   string    `json:"header"`
	Services []Preview `json:"services"`

	// ExpiresAt is the soonest any service under this id is swept - the whole
	// id's remaining life, since anything that touches one service extends
	// them all.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

// validate normalises the request into a Preview. headerName is the service's
// single header name: it is deliberately NOT per-request, because two services
// under one work id reached by different header names would defeat the point.
func (r *PreviewRequest) validate(cfg *config) (*Preview, error) {
	workID := strings.TrimSpace(r.WorkID)
	if workID == "" {
		return nil, fmt.Errorf("workId is required: it is the header value that makes every PR of one change reachable together")
	}
	if !workIDRE.MatchString(workID) {
		return nil, fmt.Errorf("workId %q must be alphanumeric, optionally with dots, dashes or underscores", workID)
	}

	workload := strings.TrimSpace(r.Workload)
	namespace := strings.TrimSpace(r.Namespace)
	if workload == "" {
		return nil, fmt.Errorf("workload is required (one service, not a repository)")
	}
	// The workload name ends up as a label value on everything created and as
	// part of every object name, so it has to be a DNS label - which every
	// Kubernetes workload name already is.
	if !nameRE.MatchString(workload) {
		return nil, fmt.Errorf("workload %q must be a DNS label", workload)
	}
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if !cfg.namespaceAllowed(namespace) {
		return nil, fmt.Errorf("namespace %q is not in agentic-preview's allowed set (%s)",
			namespace, strings.Join(cfg.allowedNamespaces, ", "))
	}

	create, err := r.createSpec(workload, namespace)
	if err != nil {
		return nil, err
	}

	svc := strings.TrimSpace(r.PreviewService)
	svcNS := strings.TrimSpace(r.PreviewNamespace)
	if create != nil {
		// Nothing to name: the preview Service is the one about to be built,
		// in the namespace the workload it is copied from lives in.
		svc, svcNS = create.Name, create.Namespace
	} else if svc == "" {
		return nil, fmt.Errorf("previewService is required when nothing is being built: " +
			"either name a Service you deployed yourself, or set image or imageTag and agentic-preview will build the preview")
	}
	if host, rest, ok := strings.Cut(svc, "."); ok {
		svc = host
		if svcNS == "" {
			svcNS, _, _ = strings.Cut(rest, ".")
		}
	}
	if svcNS == "" {
		svcNS = namespace
	}

	// Both halves of the address must be plain DNS labels. Without this a
	// caller could pass a whole FQDN as the namespace ("svc.cluster.local" and
	// friends), which resolveTarget would then leave alone instead of
	// qualifying - so the shape is checked here rather than assumed there.
	if !nameRE.MatchString(svc) {
		return nil, fmt.Errorf("previewService %q must be a DNS label, optionally as name.namespace", r.PreviewService)
	}
	if !nameRE.MatchString(svcNS) {
		return nil, fmt.Errorf("preview service namespace %q must be a DNS label", svcNS)
	}

	// The allow-list bounds where traffic is forwarded TO as well as where it
	// is intercepted. Without this check it bounded only interception: the
	// namespace can be set explicitly with previewNamespace or embedded in
	// previewService as "name.namespace", and either way it ends up as the
	// tunnel's target address, which is a plain net.Dial to any ClusterIP in
	// the cluster. Two different fields, so the message says which one.
	if !cfg.namespaceAllowed(svcNS) {
		return nil, fmt.Errorf("forward-target namespace %q is not in agentic-preview's allowed set (%s): "+
			"this is the namespace of previewService (the preview forwarded TO), not %q, the intercepted workload's namespace",
			svcNS, strings.Join(cfg.allowedNamespaces, ", "), namespace)
	}

	port := r.PreviewPort
	if port == 0 && create == nil {
		// Nothing here can read a Service that somebody else deployed, so the
		// only honest default is the conventional one. When the preview is
		// built here the port is read off the Service that was just built,
		// which is why this default does not apply to that path.
		port = 80
	}
	portID := strings.TrimSpace(r.Port)
	if portID == "" {
		portID = "80"
	}

	name := sanitise(workload + "-" + workID)
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("workload %q and workId %q produce an unusable intercept name %q", workload, workID, name)
	}

	return &Preview{
		WorkID:        workID,
		Workload:      workload,
		Namespace:     namespace,
		Name:          name,
		HeaderName:    cfg.headerName,
		HeaderValue:   workID,
		PortID:        portID,
		TargetService: svc + "." + svcNS,
		TargetPort:    port,
		Disposition:   "PENDING",
		Create:        create,
	}, nil
}

// createSpec reads the "build it for me" half of a request. It returns nil when
// the caller is bringing their own Service, which is the behaviour this service
// had before it could build anything.
func (r *PreviewRequest) createSpec(workload, namespace string) (*CreateSpec, error) {
	image := strings.TrimSpace(r.Image)
	if image == "" {
		return nil, nil
	}
	// The only thing checked about the reference is that it is made of
	// characters an image reference is made of. It is not parsed into
	// registry, repository and tag, and nothing is inferred from any of them:
	// the caller says what to run and this runs it.
	if !imageRefRE.MatchString(image) {
		return nil, fmt.Errorf("image %q is not a usable image reference", image)
	}

	// The two fields that name where to route to are meaningless when the
	// thing being routed to is the thing being built. Refusing them beats
	// silently ignoring a caller who thinks they are choosing something.
	if strings.TrimSpace(r.PreviewService) != "" {
		return nil, fmt.Errorf("previewService cannot be given alongside image: " +
			"agentic-preview names the Service it builds")
	}
	if ns := strings.TrimSpace(r.PreviewNamespace); ns != "" && ns != namespace {
		return nil, fmt.Errorf("previewNamespace %q cannot differ from namespace %q when the preview is built here: "+
			"the pod template is copied from the live workload and its ConfigMap, Secret and ServiceAccount references only resolve in its own namespace",
			ns, namespace)
	}

	source := strings.TrimSpace(r.SourceService)
	if source == "" {
		source = workload
	}
	if !nameRE.MatchString(source) {
		return nil, fmt.Errorf("sourceService %q must be a DNS label", source)
	}
	if c := strings.TrimSpace(r.Container); c != "" && !nameRE.MatchString(c) {
		return nil, fmt.Errorf("container %q must be a DNS label", c)
	}

	replicas := r.Replicas
	if replicas == 0 {
		replicas = 1
	}
	// A preview exists to be looked at, not to carry load. The cap is here so
	// that a typo in a pipeline variable cannot fill a node.
	if replicas < 0 || replicas > maxPreviewReplicas {
		return nil, fmt.Errorf("replicas must be between 1 and %d, got %d", maxPreviewReplicas, replicas)
	}

	name := sanitise(workload + "-preview-" + strings.TrimSpace(r.WorkID))
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("workload %q and workId %q produce an unusable preview name %q",
			workload, r.WorkID, name)
	}

	return &CreateSpec{
		Namespace:      namespace,
		SourceWorkload: workload,
		SourceService:  source,
		Name:           name,
		Image:          image,
		Container:      strings.TrimSpace(r.Container),
		Replicas:       replicas,
	}, nil
}

// resolveTarget turns the preview Service's name into a literal IP.
//
// This is our job, not the manager's: the traffic-agent builds the
// tunnel's connection ID from target_host with iputil.ParseAddr and fails the
// intercept on anything that is not an address. The CLI hides this by minting
// a synthetic IPv6 address and resolving it back at its own end; in-cluster
// there is nothing to hide, so resolve it and pass the ClusterIP.
func (p *Preview) resolveTarget() error {
	host := p.TargetService
	if !strings.HasSuffix(host, ".svc.cluster.local") {
		host += ".svc.cluster.local"
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolving preview service %s: %w", host, err)
	}
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip); ok {
			if a = a.Unmap(); a.Is4() {
				p.TargetHost = a.String()
				return nil
			}
		}
	}
	return fmt.Errorf("preview service %s resolved to no IPv4 address", host)
}

func sanitise(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// registry holds the desired set of previews, grouped by work id. It is the
// authority: a new manager session is reconciled to whatever is in here, which
// is what makes a manager restart survivable.
type registry struct {
	mu sync.RWMutex
	// byKey is keyed by (workId, namespace, workload) - one entry per service
	// per work id, so adding a service to an existing id never replaces it.
	byKey map[serviceKey]*Preview
}

func newRegistry() *registry {
	return &registry{byKey: map[serviceKey]*Preview{}}
}

// add inserts or updates one service under a work id. It returns an error if
// the derived intercept name is already taken by a different service, which
// would otherwise silently collide in manager state.
func (r *registry) add(p *Preview) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, ex := range r.byKey {
		if ex.Name == p.Name && k != p.key() {
			return fmt.Errorf("intercept name %q is already used by %s.%s under work id %s",
				p.Name, ex.Workload, ex.Namespace, ex.WorkID)
		}
	}
	// Age is the age of the PREVIEW, not of the last call about it. Re-raising
	// a service with a newer image is the same preview carrying on, so it
	// keeps the moment it first appeared.
	if ex, existed := r.byKey[p.key()]; existed && !ex.RaisedAt.IsZero() {
		p.RaisedAt = ex.RaisedAt
	} else if p.RaisedAt.IsZero() {
		p.RaisedAt = time.Now()
	}
	r.byKey[p.key()] = p
	return nil
}

// conflictingMode returns a live intercept on the same workload running in the
// OTHER mode, or nil.
//
// This is the check that stops the two features fighting over one target, and it
// exists because the failure it prevents is SILENT. "Has filters" is a
// per-workload switch in the traffic-agent, not a per-intercept one: one
// header-filtered intercept puts the port into HTTP mode, where the matcher has
// a header tier and a path tier and a filterless intercept qualifies for
// neither - so it is never selected, traffic goes to the real application, and
// the manager still reports it ACTIVE. In the other direction a live global
// intercept is the one the raw listener serves and a header-keyed one raised
// alongside it is equally inert. Either way nothing errors and nothing logs.
//
// So the refusal is made here, loudly, at the point where both are known.
func (r *registry) conflictingMode(p *Preview) *Preview {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, ex := range r.byKey {
		if k == p.key() || ex.agentKey() != p.agentKey() || ex.Global == p.Global {
			continue
		}
		return ex
	}
	return nil
}

func (r *registry) get(k serviceKey) (*Preview, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byKey[k]
	return p, ok
}

// removeService drops one service from one work id.
func (r *registry) removeService(k serviceKey) (*Preview, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byKey[k]
	delete(r.byKey, k)
	return p, ok
}

// removeWork drops a whole work id - every service previewed under it.
func (r *registry) removeWork(workID string) []*Preview {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Preview
	for k, p := range r.byKey {
		if k.WorkID == workID {
			out = append(out, p)
			delete(r.byKey, k)
		}
	}
	sortByName(out)
	return out
}

// works returns every work id with its service set, sorted, as copies.
func (r *registry) works() []Work {
	r.mu.RLock()
	defer r.mu.RUnlock()
	byID := map[string][]Preview{}
	header := map[string]string{}
	now := time.Now()
	for _, p := range r.byKey {
		c := *p
		if !c.RaisedAt.IsZero() {
			c.Age = now.Sub(c.RaisedAt).Round(time.Second).String()
		}
		byID[c.WorkID] = append(byID[c.WorkID], c)
		if c.Global {
			header[c.WorkID] = "(none: global intercept, all traffic to the port)"
		} else {
			header[c.WorkID] = c.HeaderName + ": " + c.HeaderValue
		}
	}
	out := make([]Work, 0, len(byID))
	for id, svcs := range byID {
		sort.Slice(svcs, func(i, j int) bool {
			if svcs[i].Namespace != svcs[j].Namespace {
				return svcs[i].Namespace < svcs[j].Namespace
			}
			return svcs[i].Workload < svcs[j].Workload
		})
		var expires time.Time
		for _, p := range svcs {
			if !p.ExpiresAt.IsZero() && (expires.IsZero() || p.ExpiresAt.Before(expires)) {
				expires = p.ExpiresAt
			}
		}
		out = append(out, Work{WorkID: id, Header: header[id], Services: svcs, ExpiresAt: expires})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkID < out[j].WorkID })
	return out
}

// work returns one work id's set, or false when the id has nothing live.
func (r *registry) work(workID string) (Work, bool) {
	for _, w := range r.works() {
		if w.WorkID == workID {
			return w, true
		}
	}
	return Work{}, false
}

// all returns the live pointers, for the reconciler.
func (r *registry) all() []*Preview {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Preview, 0, len(r.byKey))
	for _, p := range r.byKey {
		out = append(out, p)
	}
	sortByName(out)
	return out
}

// workloads returns the distinct agent keys the registry needs tunnels for.
// Two work ids on one service share a single entry, and so a single tunnel.
func (r *registry) workloads() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]bool{}
	for _, p := range r.byKey {
		out[p.agentKey()] = true
	}
	return out
}

// touch extends the life of EVERY service under a work id, because a work id
// is one change and its services are looked at together: adding the third
// repository's PR to an id means the first two are still being used.
//
// Teardown is explicit. This exists only so that a preview somebody forgot does
// not sit there forever, and expiry is reported by the API before it happens so
// that it can be extended rather than discovered.
func (r *registry) touch(workID string, until time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.byKey {
		// A scheduled intercept's life is its window and nothing else. Giving
		// it an expiry would hand the reap sweep a second opinion about when it
		// ends, and the two would disagree the moment a window ran past
		// PREVIEW_LIFETIME - which an overnight-plus-weekend window does.
		if p.WorkID == workID && p.Schedule == "" {
			p.ExpiresAt = until
		}
	}
}

// expired returns the work ids whose previews are past their expiry.
func (r *registry) expired(now time.Time) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, p := range r.byKey {
		if p.ExpiresAt.IsZero() || now.Before(p.ExpiresAt) || seen[p.WorkID] {
			continue
		}
		seen[p.WorkID] = true
		out = append(out, p.WorkID)
	}
	sort.Strings(out)
	return out
}

func (r *registry) setStatus(name, disposition, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.byKey {
		if p.Name == name {
			p.Disposition, p.Message = disposition, message
		}
	}
}

// setAgentPod records which node-agent pod carries a workload's tunnel, for
// every preview of that workload whatever its work id.
func (r *registry) setAgentPod(agentKey, podName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.byKey {
		if p.agentKey() == agentKey {
			p.AgentPod = podName
		}
	}
}

func sortByName(ps []*Preview) {
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
}
