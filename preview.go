package main

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	workIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

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

	HeaderName  string `json:"headerName"`
	HeaderValue string `json:"headerValue"`
	PortID      string `json:"port"`

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
}

func (p *Preview) key() serviceKey { return serviceKey{p.WorkID, p.Namespace, p.Workload} }

// agentKey is the identity a dial loop is kept under: one tunnel per agent pod
// per session, shared by every preview of that workload whatever its work id.
func (p *Preview) agentKey() string { return p.Workload + "." + p.Namespace }

// Work is one work id's whole set, as the list API reports it, so a person can
// see that their change spans checkout and pricing and nothing else.
type Work struct {
	WorkID   string    `json:"workId"`
	Header   string    `json:"header"`
	Services []Preview `json:"services"`
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
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if !cfg.namespaceAllowed(namespace) {
		return nil, fmt.Errorf("namespace %q is not in agentic-preview's allowed set (%s)",
			namespace, strings.Join(cfg.allowedNamespaces, ", "))
	}

	svc := strings.TrimSpace(r.PreviewService)
	if svc == "" {
		return nil, fmt.Errorf("previewService is required")
	}
	svcNS := strings.TrimSpace(r.PreviewNamespace)
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
	if port == 0 {
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
	r.byKey[p.key()] = p
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
	for _, p := range r.byKey {
		byID[p.WorkID] = append(byID[p.WorkID], *p)
		header[p.WorkID] = p.HeaderName + ": " + p.HeaderValue
	}
	out := make([]Work, 0, len(byID))
	for id, svcs := range byID {
		sort.Slice(svcs, func(i, j int) bool {
			if svcs[i].Namespace != svcs[j].Namespace {
				return svcs[i].Namespace < svcs[j].Namespace
			}
			return svcs[i].Workload < svcs[j].Workload
		})
		out = append(out, Work{WorkID: id, Header: header[id], Services: svcs})
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
