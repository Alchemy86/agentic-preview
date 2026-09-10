package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The labels every object agentic-preview creates carries. They are the whole
// ownership story: nothing is deleted, and nothing is overwritten, unless it
// carries managedByLabel=managedByValue. A stray preview - one left by a
// process that was killed before its DELETE arrived - is found with
//
//	kubectl get deploy,svc -A -l app.kubernetes.io/managed-by=agentic-preview
const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "agentic-preview"

	// workIDLabel and workloadLabel are what a DELETE selects on: a whole work
	// id, or one service within it.
	workIDLabel   = "agentic-preview/work-id"
	workloadLabel = "agentic-preview/workload"

	// previewLabel is unique per preview and is the ONLY thing the preview
	// Service selects on. Deliberately not `app`: the pod template is copied
	// wholesale from the live workload, so every label on it may already mean
	// something to something else, and the one label the preview Service
	// matches on has to be one nothing else in the namespace has ever seen.
	previewLabel = "agentic-preview/preview"
)

// imageRefRE is a character check and nothing more. agentic-preview does not
// parse an image reference into registry, repository and tag, and infers
// nothing from any of them - see the CreateSpec.Image comment for why that is
// a boundary rather than an omission.
var imageRefRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,254}$`)

// requestError marks a failure the CALLER can fix - a workload that is not
// there, a container name that is wrong, an image that will not pull. The API
// answers those with 400 and everything else with 502, so that a pipeline can
// tell "I asked for the wrong thing" from "the cluster is having a bad day"
// without reading the prose.
type requestError struct{ err error }

func (e requestError) Error() string { return e.err.Error() }
func (e requestError) Unwrap() error { return e.err }

func badRequest(format string, a ...any) error {
	return requestError{fmt.Errorf(format, a...)}
}

// CreateSpec is the "and build it for me" half of a POST /previews: everything
// needed to make the preview Deployment and Service that the intercept will
// then be pointed at. Nil when the caller brought their own Service.
type CreateSpec struct {
	// Namespace is where the preview is built. It is always the intercepted
	// workload's namespace - the pod template is copied from a Deployment
	// there and carries ConfigMap, Secret and ServiceAccount references that
	// only resolve there.
	Namespace string `json:"namespace"`

	// SourceWorkload is the live Deployment copied. SourceService is the live
	// Service whose ports are copied.
	SourceWorkload string `json:"sourceWorkload"`
	SourceService  string `json:"sourceService"`

	// Name is the preview Deployment's and Service's name.
	Name string `json:"name"`

	// Image is the reference to run, exactly as the caller gave it.
	//
	// This is the boundary the whole feature is drawn around: the tool knows
	// NOTHING about how the image came to exist or what its tag means. It does
	// not complete a bare tag against the live container's registry, does not
	// read a work id or a branch or a commit out of it, and does not assume
	// any registry. Triggering a build and knowing where it was pushed is the
	// adopter's job; running what they name is this service's.
	Image string `json:"image,omitempty"`

	// Container names which container's image to swap when the pod has more
	// than one and none is named after the Deployment.
	Container string `json:"container,omitempty"`

	Replicas int32 `json:"replicas"`
}

// CreatedObjects records what agentic-preview actually made, so the API can
// report it and a person can go and look at it.
type CreatedObjects struct {
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Service    string `json:"service"`
	Container  string `json:"container"`
	Replicas   int32  `json:"replicas"`
	Ready      bool   `json:"ready"`
}

// ownerSelector is the label selector for everything this tool made under one
// work id, optionally narrowed to one workload within it. Every delete goes
// through this: the tool never names an object it did not first find under its
// own managed-by label.
func ownerSelector(workID, workload string) string {
	sel := managedByLabel + "=" + managedByValue + "," + workIDLabel + "=" + workID
	if workload != "" {
		sel += "," + workloadLabel + "=" + workload
	}
	return sel
}

// ownedByUs is the second gate, and it is not redundant. The selector is
// evaluated by the API server; this is evaluated here. A delete that reached
// the wrong object because a selector was built wrong, or because a label
// selector was somehow dropped from the request, is stopped by this.
func ownedByUs(labels map[string]string) bool {
	return labels[managedByLabel] == managedByValue
}

func previewLabels(p *Preview, name string) map[string]string {
	return map[string]string{
		managedByLabel: managedByValue,
		workIDLabel:    p.WorkID,
		workloadLabel:  p.Workload,
		previewLabel:   name,
		"app":          name,
	}
}

// pickContainer chooses which container's image to swap, exactly as the
// reference script does: the named one, else the one named after the
// Deployment, else the only one. A pod with a sidecar and no obvious primary is
// an error rather than a guess.
func pickContainer(spec *corev1.PodSpec, deployment, override string) (*corev1.Container, error) {
	if len(spec.Containers) == 0 {
		return nil, badRequest("live Deployment %s has no containers", deployment)
	}
	names := make([]string, 0, len(spec.Containers))
	for i := range spec.Containers {
		names = append(names, spec.Containers[i].Name)
	}
	if override != "" {
		for i := range spec.Containers {
			if spec.Containers[i].Name == override {
				return &spec.Containers[i], nil
			}
		}
		return nil, badRequest("container %q is not in %s (it has: %s)", override, deployment, strings.Join(names, ", "))
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name == deployment {
			return &spec.Containers[i], nil
		}
	}
	if len(spec.Containers) == 1 {
		return &spec.Containers[0], nil
	}
	return nil, badRequest("%s has several containers (%s) and none named after it - say which one with \"container\"",
		deployment, strings.Join(names, ", "))
}

// buildPreviewDeployment copies the LIVE Deployment. That is the whole design
// and it is not an implementation detail: three earlier attempts at this built
// a Deployment from a template instead and failed three different ways - a tag
// that did not exist, no image pull credentials, and a pod that started and
// died instantly because it had none of the live configuration. Everything a
// pod needs to run is already on the live workload, so the only honest way to
// build a copy of it is to copy it.
//
// Copied by virtue of copying the pod template, without naming any of them:
// env, envFrom, volumes and volumeMounts, imagePullSecrets, serviceAccountName,
// probes, resources, securityContext, affinity, tolerations, topology spread.
//
// Changed deliberately, and this is the complete list:
//
//	image        on the chosen container, verbatim as the caller gave it
//	replicas     as asked
//	labels       ours added, `app` pointed at the preview
//	annotations  telepresence's own and kubectl's restartedAt dropped
//	containers   the injected traffic-agent and its init container dropped
//	readiness    replaced with a copy of the liveness probe, or removed
//
// The readiness swap is the one that needs a reason. A live workload's
// readiness probe is frequently the deep one - it checks the database, the
// queue, a downstream API - because that is what should take a live pod out of
// rotation. A preview that fails it never becomes ready and the preview looks
// broken when the code is fine. The liveness probe is the shallow "is this
// process answering" check, which is the right question to ask of a preview.
func buildPreviewDeployment(live *appsv1.Deployment, spec *CreateSpec, p *Preview) (*appsv1.Deployment, string, error) {
	tmpl := *live.Spec.Template.DeepCopy()

	if tmpl.Labels == nil {
		tmpl.Labels = map[string]string{}
	}
	for k, v := range previewLabels(p, spec.Name) {
		tmpl.Labels[k] = v
	}

	for k := range tmpl.Annotations {
		if strings.HasPrefix(k, "telepresence.getambassador.io/") || k == "kubectl.kubernetes.io/restartedAt" {
			delete(tmpl.Annotations, k)
		}
	}
	if len(tmpl.Annotations) == 0 {
		tmpl.Annotations = nil
	}

	// The live workload may itself be intercepted right now, in which case its
	// pod template carries telepresence's injected sidecar. Copying that into
	// the preview would put a second traffic-agent in the cluster claiming to
	// be the same workload.
	tmpl.Spec.Containers = dropContainer(tmpl.Spec.Containers, "traffic-agent")
	tmpl.Spec.InitContainers = dropContainer(tmpl.Spec.InitContainers, "tel-agent-init")

	target, err := pickContainer(&tmpl.Spec, spec.SourceWorkload, spec.Container)
	if err != nil {
		return nil, "", err
	}

	// Verbatim. Not completed, not normalised, not inspected.
	target.Image = spec.Image

	if target.LivenessProbe != nil {
		target.ReadinessProbe = target.LivenessProbe.DeepCopy()
	} else {
		target.ReadinessProbe = nil
	}

	replicas := spec.Replicas
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: spec.Namespace,
			Labels:    previewLabels(p, spec.Name),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{previewLabel: spec.Name}},
			Template: tmpl,
		},
	}, target.Name, nil
}

func dropContainer(cs []corev1.Container, name string) []corev1.Container {
	out := cs[:0:0]
	for _, c := range cs {
		if c.Name != name {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildPreviewService copies the live Service's ports so that the preview
// answers on the same port numbers and names the live one does - the intercept
// forwards to a port identifier taken from the live side, so they have to
// agree. Everything else is ours: a ClusterIP selecting only preview pods.
func buildPreviewService(live *corev1.Service, spec *CreateSpec, p *Preview) *corev1.Service {
	ports := make([]corev1.ServicePort, 0, len(live.Spec.Ports))
	for _, port := range live.Spec.Ports {
		port.NodePort = 0
		ports = append(ports, port)
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: spec.Namespace,
			Labels:    previewLabels(p, spec.Name),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{previewLabel: spec.Name},
			Ports:    ports,
		},
	}
}

// servicePort picks the port the intercept should forward to, the same way the
// reference script does: the one called "http", else 80, else the first.
func servicePort(svc *corev1.Service) int {
	for _, p := range svc.Spec.Ports {
		if p.Name == "http" {
			return int(p.Port)
		}
	}
	for _, p := range svc.Spec.Ports {
		if p.Port == 80 {
			return int(p.Port)
		}
	}
	if len(svc.Spec.Ports) > 0 {
		return int(svc.Spec.Ports[0].Port)
	}
	return 80
}

// capturedBy is the isolation check, and it is the most important thing in this
// file after the copy itself.
//
// The preview's pod template is copied from the live workload, so it arrives
// carrying every label the live pods carry - including, very likely, the exact
// labels the LIVE Service selects on. A pod like that joins the live Service's
// EndpointSlice and unreviewed code starts serving live traffic, silently, to
// everybody. The reference script scans EndpointSlices after the fact and tells
// you to tear the preview down; there is no reason to do it after the fact when
// the pod labels and every Service's selector are both readable beforehand.
//
// So: before creating anything, check the preview's pod labels against the
// selector of every Service in the namespace, and refuse if any of them would
// claim it. Services with no selector select nothing and are skipped; so are
// our own previews.
func capturedBy(services []corev1.Service, previewName string, podLabels map[string]string) []string {
	var hits []string
	for i := range services {
		svc := &services[i]
		if svc.Name == previewName || ownedByUs(svc.Labels) || len(svc.Spec.Selector) == 0 {
			continue
		}
		matched := true
		for k, v := range svc.Spec.Selector {
			if podLabels[k] != v {
				matched = false
				break
			}
		}
		if matched {
			keys := make([]string, 0, len(svc.Spec.Selector))
			for k, v := range svc.Spec.Selector {
				keys = append(keys, k+"="+v)
			}
			sort.Strings(keys)
			hits = append(hits, fmt.Sprintf("%s (selector %s)", svc.Name, strings.Join(keys, ",")))
		}
	}
	sort.Strings(hits)
	return hits
}

// createWorkload builds the preview Deployment and Service from the live ones
// and applies them. It is idempotent: a pipeline retry updates in place rather
// than colliding, which is also how a work id re-raised with a newer image tag
// rolls forward without a delete/create gap.
func (s *Server) createWorkload(ctx context.Context, p *Preview) error {
	spec := p.Create

	// The allow-list gates creation exactly as it gates interception and
	// forwarding. validate() already checked it; this is checked again here so
	// that the check cannot be lost by a future caller reaching this function
	// another way. Creating a workload is the most consequential thing this
	// service does and it gets the boundary stated at the point of use.
	if !s.cfg.namespaceAllowed(spec.Namespace) {
		return badRequest("refusing to create a preview in namespace %q: it is not in agentic-preview's allowed set (%s)",
			spec.Namespace, strings.Join(s.cfg.allowedNamespaces, ", "))
	}
	if s.kube == nil {
		return fmt.Errorf("agentic-preview has no Kubernetes API client (%v), so it cannot create a preview: "+
			"deploy the preview yourself and POST previewService instead", s.kubeErr)
	}

	live, err := s.kube.getDeployment(ctx, spec.Namespace, spec.SourceWorkload)
	if err != nil {
		if isNotFound(err) {
			return badRequest("no Deployment %s in namespace %s to copy the preview from",
				spec.SourceWorkload, spec.Namespace)
		}
		return fmt.Errorf("reading live Deployment %s.%s: %w", spec.SourceWorkload, spec.Namespace, err)
	}
	liveSvc, err := s.kube.getService(ctx, spec.Namespace, spec.SourceService)
	if err != nil {
		if isNotFound(err) {
			return badRequest("no Service %s in namespace %s to copy the preview's ports from - "+
				"name it with \"sourceService\" if the Service is not called the same as the workload",
				spec.SourceService, spec.Namespace)
		}
		return fmt.Errorf("reading live Service %s.%s: %w", spec.SourceService, spec.Namespace, err)
	}

	want, container, err := buildPreviewDeployment(live, spec, p)
	if err != nil {
		return err
	}

	all, err := s.kube.listServices(ctx, spec.Namespace, "")
	if err != nil {
		return fmt.Errorf("listing Services in %s to check the preview is isolated from live traffic: %w", spec.Namespace, err)
	}
	if hits := capturedBy(all, spec.Name, want.Spec.Template.Labels); len(hits) > 0 {
		return badRequest("refusing to create %s: its pod labels are copied from %s and would still be selected by %s, "+
			"which would put this build into live traffic. Change the label those Services select on, or deploy the preview "+
			"yourself and POST previewService instead",
			spec.Name, spec.SourceWorkload, strings.Join(hits, "; "))
	}

	wantSvc := buildPreviewService(liveSvc, spec, p)

	if err := s.applyDeployment(ctx, want); err != nil {
		return err
	}
	created, err := s.applyService(ctx, wantSvc)
	if err != nil {
		return err
	}

	if p.TargetPort == 0 {
		p.TargetPort = servicePort(created)
	}
	p.Image = spec.Image
	p.Created = &CreatedObjects{
		Namespace:  spec.Namespace,
		Deployment: want.Name,
		Service:    created.Name,
		Container:  container,
		Replicas:   spec.Replicas,
	}
	logf("%s: preview %s.%s created from live %s (%s -> %s), %d replica(s)",
		p.Name, spec.Name, spec.Namespace, spec.SourceWorkload, container, spec.Image, spec.Replicas)

	ready, detail := s.waitReady(ctx, spec.Namespace, spec.Name, spec.Replicas)
	p.Created.Ready = ready
	if !ready {
		return badRequest("preview %s.%s was created but never became ready, so no intercept was raised: %s. "+
			"The objects are left in place to look at; DELETE /previews/%s removes them",
			spec.Name, spec.Namespace, detail, p.WorkID)
	}
	return nil
}

// applyDeployment creates the preview Deployment, or updates it in place when a
// work id is re-raised. The update is label-guarded: an existing object without
// our managed-by label is somebody else's and is never written to.
func (s *Server) applyDeployment(ctx context.Context, want *appsv1.Deployment) error {
	live, err := s.kube.getDeployment(ctx, want.Namespace, want.Name)
	if err != nil {
		if !isNotFound(err) {
			return fmt.Errorf("reading %s.%s: %w", want.Name, want.Namespace, err)
		}
		if _, err := s.kube.createDeployment(ctx, want); err != nil {
			return fmt.Errorf("creating Deployment %s.%s: %w", want.Name, want.Namespace, err)
		}
		return nil
	}
	if !ownedByUs(live.Labels) {
		return badRequest("Deployment %s.%s already exists and was not created by agentic-preview - refusing to touch it",
			want.Name, want.Namespace)
	}
	want.ResourceVersion = live.ResourceVersion
	if _, err := s.kube.updateDeployment(ctx, want); err != nil {
		return fmt.Errorf("updating Deployment %s.%s: %w", want.Name, want.Namespace, err)
	}
	return nil
}

func (s *Server) applyService(ctx context.Context, want *corev1.Service) (*corev1.Service, error) {
	live, err := s.kube.getService(ctx, want.Namespace, want.Name)
	if err != nil {
		if !isNotFound(err) {
			return nil, fmt.Errorf("reading Service %s.%s: %w", want.Name, want.Namespace, err)
		}
		out, err := s.kube.createService(ctx, want)
		if err != nil {
			return nil, fmt.Errorf("creating Service %s.%s: %w", want.Name, want.Namespace, err)
		}
		return out, nil
	}
	if !ownedByUs(live.Labels) {
		return nil, badRequest("Service %s.%s already exists and was not created by agentic-preview - refusing to touch it",
			want.Name, want.Namespace)
	}
	// ClusterIP is immutable; carry the assigned one across so the update is
	// accepted, and so the intercept keeps pointing at the same address.
	want.ResourceVersion = live.ResourceVersion
	want.Spec.ClusterIP = live.Spec.ClusterIP
	want.Spec.ClusterIPs = live.Spec.ClusterIPs
	out, err := s.kube.updateService(ctx, want)
	if err != nil {
		return nil, fmt.Errorf("updating Service %s.%s: %w", want.Name, want.Namespace, err)
	}
	return out, nil
}

// waitReady blocks until the preview's pods are actually running, because the
// two most common ways a preview fails - a tag that does not exist and no
// credentials to pull it - are invisible at create time and obvious ten seconds
// later. Raising an intercept at a Service with no endpoints turns a broken
// build into a hanging header, which is a much worse way to find out.
func (s *Server) waitReady(ctx context.Context, ns, name string, replicas int32) (bool, string) {
	if s.cfg.readyTimeout <= 0 {
		return true, "not waited for (PREVIEW_READY_TIMEOUT=0)"
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.readyTimeout)
	defer cancel()

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		d, err := s.kube.getDeployment(ctx, ns, name)
		if err == nil && d.Status.ObservedGeneration >= d.Generation &&
			d.Status.ReadyReplicas >= replicas && d.Status.UpdatedReplicas >= replicas {
			return true, ""
		}
		select {
		case <-ctx.Done():
			return false, strings.Join(s.podTrouble(context.WithoutCancel(ctx), ns, name), "; ")
		case <-tick.C:
		}
	}
}

// podTrouble reports why the preview's pods are not running, in the words
// Kubernetes uses - "ErrImagePull: manifest unknown" and "ImagePullBackOff" are
// the two the caller most needs to see, and they name the two failures this
// whole design exists to avoid.
func (s *Server) podTrouble(ctx context.Context, ns, name string) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pods, err := s.kube.listPods(ctx, ns, previewLabel+"="+name)
	if err != nil {
		return []string{fmt.Sprintf("could not read the preview's pods: %v", err)}
	}
	if len(pods) == 0 {
		return []string{"no pods were scheduled for it at all"}
	}
	var out []string
	for i := range pods {
		pod := &pods[i]
		line := fmt.Sprintf("%s is %s", pod.Name, pod.Status.Phase)
		for _, cs := range pod.Status.ContainerStatuses {
			switch {
			case cs.State.Waiting != nil:
				line += fmt.Sprintf(" - %s waiting: %s %s", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
			case cs.State.Terminated != nil:
				line += fmt.Sprintf(" - %s terminated: %s (exit %d)", cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
			}
			if cs.RestartCount > 0 {
				line += fmt.Sprintf(" - %s has restarted %d time(s)", cs.Name, cs.RestartCount)
			}
		}
		out = append(out, strings.TrimSpace(line))
	}
	sort.Strings(out)
	return out
}

// dropWorkload deletes every object agentic-preview created under one work id -
// optionally narrowed to one workload - across the allowed namespaces, and
// NOTHING else.
//
// Two things make that true rather than intended. The list is by the tool's own
// managed-by label, so an object it did not create is never returned; and every
// name it is about to delete is re-checked against that label, so an object it
// did not create is never named even if the selector somehow were not applied.
// It deletes by name, never by collection, so there is no request that could
// widen.
func (s *Server) dropWorkload(ctx context.Context, workID, workload string) (removed, problems []string) {
	if s.kube == nil {
		return nil, nil
	}
	selector := ownerSelector(workID, workload)
	for _, ns := range s.cfg.allowedNamespaces {
		deps, err := s.kube.listDeployments(ctx, ns, selector)
		if err != nil {
			problems = append(problems, fmt.Sprintf("listing preview Deployments in %s: %v", ns, err))
		}
		for i := range deps {
			d := &deps[i]
			if !ownedByUs(d.Labels) {
				problems = append(problems, fmt.Sprintf("deployment/%s.%s matched the selector but does not carry %s=%s - left alone",
					d.Name, ns, managedByLabel, managedByValue))
				continue
			}
			if err := s.kube.deleteDeployment(ctx, ns, d.Name); err != nil {
				problems = append(problems, fmt.Sprintf("deleting deployment/%s.%s: %v", d.Name, ns, err))
				continue
			}
			removed = append(removed, "deployment/"+d.Name+"."+ns)
		}

		svcs, err := s.kube.listServices(ctx, ns, selector)
		if err != nil {
			problems = append(problems, fmt.Sprintf("listing preview Services in %s: %v", ns, err))
		}
		for i := range svcs {
			svc := &svcs[i]
			if !ownedByUs(svc.Labels) {
				problems = append(problems, fmt.Sprintf("service/%s.%s matched the selector but does not carry %s=%s - left alone",
					svc.Name, ns, managedByLabel, managedByValue))
				continue
			}
			if err := s.kube.deleteService(ctx, ns, svc.Name); err != nil {
				problems = append(problems, fmt.Sprintf("deleting service/%s.%s: %v", svc.Name, ns, err))
				continue
			}
			removed = append(removed, "service/"+svc.Name+"."+ns)
		}
	}
	sort.Strings(removed)
	return removed, problems
}

// tearDownWork removes everything one work id has: its intercepts, and every
// object agentic-preview built for it. It is the one path both DELETE and the
// expiry sweep go through, so that a preview that times out goes exactly the
// same way as one somebody asked to remove.
func (s *Server) tearDownWork(ctx context.Context, workID string) (removed, deleted, problems []string, found bool) {
	ps := s.reg.removeWork(workID)
	removed = make([]string, 0, len(ps))
	for _, p := range ps {
		if err := s.remove(ctx, p); err != nil {
			problems = append(problems, err.Error())
		}
		removed = append(removed, p.Workload+"."+p.Namespace)
	}

	// Then whatever this tool BUILT for the work id, found by its own labels
	// rather than by what the registry remembers. The registry is in memory,
	// so a restart forgets every preview while the Deployments it created stay
	// up - and this is what lets a DELETE still clean them.
	deleted, kubeProblems := s.dropWorkload(ctx, workID, "")
	problems = append(problems, kubeProblems...)
	return removed, deleted, problems, len(ps) > 0 || len(deleted) > 0
}

// ReapExpired sweeps previews nothing has touched for PREVIEW_LIFETIME.
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
// configuration.
func (s *Server) ReapExpired(ctx context.Context) {
	if s.cfg.lifetime <= 0 {
		logf("preview expiry is OFF (PREVIEW_LIFETIME); previews live until somebody deletes them. " +
			"GET /previews reports every preview's age, which is what to watch instead")
		return
	}
	logf("previews expire %s after they are last touched", s.cfg.lifetime)
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
			removed, deleted, problems, _ := s.tearDownWork(ctx, workID)
			logf("work id %s expired: intercepts %v, objects %v, problems %v", workID, removed, deleted, problems)
		}
	}
}
