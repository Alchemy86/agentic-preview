package main

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeKube is a Kubernetes API that only does what kubeAPI says it does, so a
// test can put objects in front of the code and see exactly which ones it
// touches. Deletes are recorded rather than performed, because "what did it
// delete" is the whole question in half of these tests.
type fakeKube struct {
	deployments []appsv1.Deployment
	services    []corev1.Service
	pods        []corev1.Pod
	configMaps  []corev1.ConfigMap

	deletedDeployments []string
	deletedServices    []string
	created            []string
	updated            []string
}

// The two ConfigMap methods exist because kubeAPI has them for DNS-redirect
// schedules. Nothing in the workload path touches a ConfigMap; the DNS path is
// tested against client-go's own fake clientset in dns_test.go, through the
// real clusterKube, so that what it proves is the code that actually runs.
func (f *fakeKube) getConfigMap(_ context.Context, ns, name string) (*corev1.ConfigMap, error) {
	for i := range f.configMaps {
		if f.configMaps[i].Namespace == ns && f.configMaps[i].Name == name {
			return f.configMaps[i].DeepCopy(), nil
		}
	}
	return nil, notFound("configmaps", name)
}

func (f *fakeKube) updateConfigMap(_ context.Context, cm *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	f.updated = append(f.updated, "configmap/"+cm.Name+"."+cm.Namespace)
	for i := range f.configMaps {
		if f.configMaps[i].Namespace == cm.Namespace && f.configMaps[i].Name == cm.Name {
			f.configMaps[i] = *cm.DeepCopy()
			return cm, nil
		}
	}
	return nil, notFound("configmaps", cm.Name)
}

func notFound(resource, name string) error {
	return apierrors.NewNotFound(schema.GroupResource{Resource: resource}, name)
}

func selects(selector string, l map[string]string) bool {
	if selector == "" {
		return true
	}
	sel, err := labels.Parse(selector)
	if err != nil {
		return false
	}
	return sel.Matches(labels.Set(l))
}

func (f *fakeKube) getDeployment(_ context.Context, ns, name string) (*appsv1.Deployment, error) {
	for i := range f.deployments {
		if f.deployments[i].Namespace == ns && f.deployments[i].Name == name {
			return f.deployments[i].DeepCopy(), nil
		}
	}
	return nil, notFound("deployments", name)
}

func (f *fakeKube) createDeployment(_ context.Context, d *appsv1.Deployment) (*appsv1.Deployment, error) {
	f.created = append(f.created, "deployment/"+d.Name+"."+d.Namespace)
	f.deployments = append(f.deployments, *d.DeepCopy())
	return d, nil
}

func (f *fakeKube) updateDeployment(_ context.Context, d *appsv1.Deployment) (*appsv1.Deployment, error) {
	f.updated = append(f.updated, "deployment/"+d.Name+"."+d.Namespace)
	for i := range f.deployments {
		if f.deployments[i].Namespace == d.Namespace && f.deployments[i].Name == d.Name {
			f.deployments[i] = *d.DeepCopy()
		}
	}
	return d, nil
}

func (f *fakeKube) listDeployments(_ context.Context, ns, selector string) ([]appsv1.Deployment, error) {
	var out []appsv1.Deployment
	for i := range f.deployments {
		if f.deployments[i].Namespace == ns && selects(selector, f.deployments[i].Labels) {
			out = append(out, f.deployments[i])
		}
	}
	return out, nil
}

func (f *fakeKube) deleteDeployment(_ context.Context, ns, name string) error {
	f.deletedDeployments = append(f.deletedDeployments, name+"."+ns)
	return nil
}

func (f *fakeKube) getService(_ context.Context, ns, name string) (*corev1.Service, error) {
	for i := range f.services {
		if f.services[i].Namespace == ns && f.services[i].Name == name {
			return f.services[i].DeepCopy(), nil
		}
	}
	return nil, notFound("services", name)
}

func (f *fakeKube) createService(_ context.Context, svc *corev1.Service) (*corev1.Service, error) {
	f.created = append(f.created, "service/"+svc.Name+"."+svc.Namespace)
	f.services = append(f.services, *svc.DeepCopy())
	return svc, nil
}

func (f *fakeKube) updateService(_ context.Context, svc *corev1.Service) (*corev1.Service, error) {
	f.updated = append(f.updated, "service/"+svc.Name+"."+svc.Namespace)
	return svc, nil
}

func (f *fakeKube) listServices(_ context.Context, ns, selector string) ([]corev1.Service, error) {
	var out []corev1.Service
	for i := range f.services {
		if f.services[i].Namespace == ns && selects(selector, f.services[i].Labels) {
			out = append(out, f.services[i])
		}
	}
	return out, nil
}

func (f *fakeKube) deleteService(_ context.Context, ns, name string) error {
	f.deletedServices = append(f.deletedServices, name+"."+ns)
	return nil
}

func (f *fakeKube) listPods(_ context.Context, ns, selector string) ([]corev1.Pod, error) {
	var out []corev1.Pod
	for i := range f.pods {
		if f.pods[i].Namespace == ns && selects(selector, f.pods[i].Labels) {
			out = append(out, f.pods[i])
		}
	}
	return out, nil
}

// liveDeployment is a workload with everything on it that a preview built from
// a template rather than from the live object would lose. Each field here is
// one of the ways the three earlier attempts at this failed.
func liveDeployment() *appsv1.Deployment {
	one := int32(3)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-api", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "checkout-api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "checkout-api", "tier": "backend"},
					Annotations: map[string]string{
						"telepresence.getambassador.io/inject-traffic-agent": "enabled",
						"kubectl.kubernetes.io/restartedAt":                  "2026-01-01T00:00:00Z",
						"prometheus.io/scrape":                               "true",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "checkout-api",
					ImagePullSecrets:   []corev1.LocalObjectReference{{Name: "registry-pull"}},
					Volumes:            []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
					InitContainers:     []corev1.Container{{Name: "tel-agent-init", Image: "tel/init:1"}},
					Containers: []corev1.Container{
						{
							Name:  "checkout-api",
							Image: "registry.example.com/checkout-api:live",
							Env:   []corev1.EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
							EnvFrom: []corev1.EnvFromSource{
								{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "checkout-config"}}},
								{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "checkout-secrets"}}},
							},
							VolumeMounts: []corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
							},
							LivenessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz"}}},
							ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready-deep-check-of-the-database"}}},
						},
						{Name: "traffic-agent", Image: "tel/agent:1"},
					},
				},
			},
		},
	}
}

func liveService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-api", Namespace: "shop"},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.0.0.7",
			Selector:  map[string]string{"app": "checkout-api"},
			Ports:     []corev1.ServicePort{{Name: "http", Port: 8080, NodePort: 31000}},
		},
	}
}

func serverFor(k kubeAPI, namespaces ...string) *Server {
	return &Server{cfg: cfgFor(namespaces...), reg: newRegistry(), kube: k}
}

func mustValidate(t *testing.T, cfg *config, r PreviewRequest) *Preview {
	t.Helper()
	p, err := r.validate(cfg)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	return p
}

// TestCreateIsRefusedOutsideTheAllowList is the safety boundary for the half of
// the service that can now make workloads. ALLOWED_NAMESPACES already bounded
// where traffic is intercepted and where it is forwarded; creating a Deployment
// is the third thing it has to bound, and it is the one with consequences that
// outlive the request.
//
// It is checked in two places on purpose and both are asserted here: in
// validate, so a caller gets a clear 400 with the list in it, and again in
// createWorkload at the point of use, so the check cannot be lost by some
// future path into that function.
func TestCreateIsRefusedOutsideTheAllowList(t *testing.T) {
	cfg := cfgFor("shop")

	t.Run("validate refuses the request", func(t *testing.T) {
		req := PreviewRequest{
			WorkID: "1234", Workload: "some-api", Namespace: "kube-system",
			Image: "registry.example.com/some-api:pr-1234",
		}
		_, err := req.validate(cfg)
		if err == nil {
			t.Fatal("a create in kube-system was accepted")
		}
		for _, want := range []string{"kube-system", "not in", "shop"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal does not mention %q: %v", want, err)
			}
		}
	})

	// The same request, forced past validate: nothing is created and nothing
	// is even read, because the check comes first.
	t.Run("createWorkload refuses it again at the point of use", func(t *testing.T) {
		k := &fakeKube{}
		s := serverFor(k, "shop")
		p := &Preview{WorkID: "1234", Workload: "some-api", Namespace: "kube-system"}
		p.Create = &CreateSpec{
			Namespace: "kube-system", SourceWorkload: "some-api", SourceService: "some-api",
			Name: "some-api-preview-1234", Image: "registry.example.com/some-api:pr-1234", Replicas: 1,
		}
		err := s.createWorkload(context.Background(), p)
		if err == nil {
			t.Fatal("createWorkload built something in a namespace that is not on the allow-list")
		}
		if !strings.Contains(err.Error(), "kube-system") {
			t.Errorf("refusal does not name the namespace: %v", err)
		}
		if len(k.created) != 0 {
			t.Errorf("it created %v before refusing", k.created)
		}
	})
}

// TestDeleteRemovesOnlyItsOwnObjects is the other half of the boundary. The
// tool now holds delete on Deployments and Services in namespaces full of
// somebody else's workloads, so "it deletes only what it made" has to be a
// property of the code and not of the request that happens to arrive.
//
// The fake deliberately returns an object that does NOT carry the managed-by
// label from a selector that should have excluded it - a broken selector, in
// other words - to prove the second gate in dropWorkload is real.
func TestDeleteRemovesOnlyItsOwnObjects(t *testing.T) {
	ours := map[string]string{
		managedByLabel: managedByValue,
		workIDLabel:    "1234",
		workloadLabel:  "checkout-api",
		previewLabel:   "checkout-api-preview-1234",
	}
	otherWork := map[string]string{
		managedByLabel: managedByValue,
		workIDLabel:    "9999",
		workloadLabel:  "checkout-api",
	}
	k := &fakeKube{
		deployments: []appsv1.Deployment{
			{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api-preview-1234", Namespace: "shop", Labels: ours}},
			{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api-preview-9999", Namespace: "shop", Labels: otherWork}},
			{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api", Namespace: "shop", Labels: map[string]string{"app": "checkout-api"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pricing-api", Namespace: "shop"}},
			// Same work id, but in a namespace that is not on the allow-list.
			{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere-preview-1234", Namespace: "kube-system", Labels: ours}},
		},
		services: []corev1.Service{
			{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api-preview-1234", Namespace: "shop", Labels: ours}},
			{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api", Namespace: "shop", Labels: map[string]string{"app": "checkout-api"}}},
		},
	}
	s := serverFor(k, "shop")

	removed, problems := s.dropWorkload(context.Background(), "1234", "")
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}

	wantRemoved := []string{"deployment/checkout-api-preview-1234.shop", "service/checkout-api-preview-1234.shop"}
	if strings.Join(removed, " ") != strings.Join(wantRemoved, " ") {
		t.Fatalf("removed the wrong set\n  want: %v\n  got:  %v", wantRemoved, removed)
	}
	if got := strings.Join(k.deletedDeployments, " "); got != "checkout-api-preview-1234.shop" {
		t.Fatalf("deleted the wrong Deployments: %v", k.deletedDeployments)
	}
	if got := strings.Join(k.deletedServices, " "); got != "checkout-api-preview-1234.shop" {
		t.Fatalf("deleted the wrong Services: %v", k.deletedServices)
	}
}

// The label selector is evaluated by the API server, which this code does not
// control. If it ever came back with something that is not ours - a selector
// built wrong, a field dropped in transit - the object must survive and the
// caller must be told.
func TestDeleteRefusesAnObjectThatIsNotLabelledOurs(t *testing.T) {
	k := &fakeKube{
		deployments: []appsv1.Deployment{
			{ObjectMeta: metav1.ObjectMeta{Name: "somebody-elses-api", Namespace: "shop",
				Labels: map[string]string{workIDLabel: "1234"}}},
		},
	}
	// A fake whose list ignores the selector entirely is the shape of the
	// accident being guarded against.
	s := serverFor(brokenSelectorKube{k}, "shop")

	removed, problems := s.dropWorkload(context.Background(), "1234", "")
	if len(removed) != 0 {
		t.Fatalf("it deleted an object it did not create: %v", removed)
	}
	if len(k.deletedDeployments) != 0 {
		t.Fatalf("delete was called on %v", k.deletedDeployments)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "left alone") {
		t.Fatalf("the caller was not told: %v", problems)
	}
}

type brokenSelectorKube struct{ *fakeKube }

func (b brokenSelectorKube) listDeployments(ctx context.Context, ns, _ string) ([]appsv1.Deployment, error) {
	return b.fakeKube.listDeployments(ctx, ns, "")
}

// TestPreviewIsBuiltFromTheLiveWorkload is the rule the reference script earned
// the hard way: everything a pod needs to run is already on the live workload,
// so the preview is a copy of it and not a construction.
func TestPreviewIsBuiltFromTheLiveWorkload(t *testing.T) {
	cfg := cfgFor("shop")
	p := mustValidate(t, cfg, PreviewRequest{
		WorkID: "1234", Workload: "checkout-api", Namespace: "shop",
		Image: "registry.example.com/checkout-api:pr-1234",
	})

	d, container, err := buildPreviewDeployment(liveDeployment(), p.Create, p)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if container != "checkout-api" {
		t.Fatalf("swapped the wrong container: %s", container)
	}

	pod := d.Spec.Template.Spec
	var c *corev1.Container
	for i := range pod.Containers {
		if pod.Containers[i].Name == "checkout-api" {
			c = &pod.Containers[i]
		}
	}
	if c == nil {
		t.Fatal("the app container did not survive the copy")
	}

	// The image, verbatim - the one thing that changes.
	if c.Image != "registry.example.com/checkout-api:pr-1234" {
		t.Errorf("image is %q, want the caller's reference unchanged", c.Image)
	}

	// The three failures that made this rule.
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "registry-pull" {
		t.Errorf("pull credentials were not carried across: %v", pod.ImagePullSecrets)
	}
	if len(c.Env) != 1 || len(c.EnvFrom) != 2 {
		t.Errorf("configuration was not carried across: env=%v envFrom=%v", c.Env, c.EnvFrom)
	}
	if pod.ServiceAccountName != "checkout-api" {
		t.Errorf("service account was not carried across: %q", pod.ServiceAccountName)
	}
	if len(pod.Volumes) != 1 || len(c.VolumeMounts) != 1 {
		t.Errorf("volumes were not carried across")
	}
	if c.Resources.Requests.Cpu().IsZero() {
		t.Errorf("resource requests were not carried across")
	}

	// Deliberately changed.
	if *d.Spec.Replicas != 1 {
		t.Errorf("replicas is %d, want the 1 that was asked for rather than live's 3", *d.Spec.Replicas)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("readiness was not replaced with the shallow liveness check: %v", c.ReadinessProbe)
	}
	for _, name := range []string{"traffic-agent"} {
		for _, got := range pod.Containers {
			if got.Name == name {
				t.Errorf("the injected %s was copied into the preview", name)
			}
		}
	}
	if len(pod.InitContainers) != 0 {
		t.Errorf("the telepresence init container was copied into the preview: %v", pod.InitContainers)
	}
	if _, still := d.Spec.Template.Annotations["telepresence.getambassador.io/inject-traffic-agent"]; still {
		t.Errorf("telepresence's own annotations were copied into the preview")
	}
	if d.Spec.Template.Annotations["prometheus.io/scrape"] != "true" {
		t.Errorf("an unrelated annotation was dropped")
	}

	// Ownership, and the label the preview Service selects on.
	if !ownedByUs(d.Labels) || d.Labels[workIDLabel] != "1234" || d.Labels[workloadLabel] != "checkout-api" {
		t.Errorf("the preview is not unambiguously labelled as ours: %v", d.Labels)
	}
	if d.Spec.Selector.MatchLabels[previewLabel] != "checkout-api-preview-1234" {
		t.Errorf("the Deployment selects on the wrong label: %v", d.Spec.Selector.MatchLabels)
	}
	if d.Spec.Template.Labels[previewLabel] != "checkout-api-preview-1234" {
		t.Errorf("the pod does not carry the label its Deployment selects on")
	}
}

// TestPreviewIsRefusedWhenItWouldJoinLiveTraffic pins the isolation check. The
// pod template is copied from live, so it arrives carrying live's labels - and
// a preview that is still selected by the live Service serves unreviewed code
// to everybody, silently.
func TestPreviewIsRefusedWhenItWouldJoinLiveTraffic(t *testing.T) {
	cfg := cfgFor("shop")
	p := mustValidate(t, cfg, PreviewRequest{
		WorkID: "1234", Workload: "checkout-api", Namespace: "shop",
		Image: "registry.example.com/checkout-api:pr-1234",
	})

	live := liveDeployment()
	svc := liveService()
	// A Service selecting on a label the copy keeps, rather than on `app`,
	// which the copy overwrites.
	svc.Spec.Selector = map[string]string{"tier": "backend"}

	k := &fakeKube{
		deployments: []appsv1.Deployment{*live},
		services:    []corev1.Service{*svc},
	}
	s := serverFor(k, "shop")

	err := s.createWorkload(context.Background(), p)
	if err == nil {
		t.Fatal("it created a preview that the live Service would have put straight into live traffic")
	}
	if !strings.Contains(err.Error(), "live traffic") || !strings.Contains(err.Error(), "tier=backend") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
	if len(k.created) != 0 {
		t.Errorf("it created %v anyway", k.created)
	}
}

// The same shape, but the live Service selects on `app`, which the copy
// overwrites - the ordinary case, which must go through and produce both
// objects and a target the intercept can be pointed at.
func TestCreateBuildsBothObjectsAndFillsTheTargetPort(t *testing.T) {
	cfg := cfgFor("shop")
	cfg.readyTimeout = 0 // the readiness wait is exercised against a real cluster, not here
	p := mustValidate(t, cfg, PreviewRequest{
		WorkID: "1234", Workload: "checkout-api", Namespace: "shop",
		Image: "registry.example.com/checkout-api:pr-1234",
	})

	k := &fakeKube{
		deployments: []appsv1.Deployment{*liveDeployment()},
		services:    []corev1.Service{*liveService()},
	}
	s := serverFor(k, "shop")
	s.cfg = cfg

	if err := s.createWorkload(context.Background(), p); err != nil {
		t.Fatalf("createWorkload: %v", err)
	}

	want := []string{"deployment/checkout-api-preview-1234.shop", "service/checkout-api-preview-1234.shop"}
	if strings.Join(k.created, " ") != strings.Join(want, " ") {
		t.Fatalf("created\n  want: %v\n  got:  %v", want, k.created)
	}
	if p.TargetService != "checkout-api-preview-1234.shop" {
		t.Errorf("the intercept points at %q", p.TargetService)
	}
	// Read off the Service that was just built, not defaulted to 80.
	if p.TargetPort != 8080 {
		t.Errorf("target port is %d, want the live Service's 8080", p.TargetPort)
	}
	if p.Created == nil || p.Created.Deployment != "checkout-api-preview-1234" {
		t.Errorf("what was created was not recorded: %+v", p.Created)
	}
	// Reported so an adopter's image-retention policy can skip images a live
	// preview is running.
	if p.Image != "registry.example.com/checkout-api:pr-1234" {
		t.Errorf("the preview does not report the image it is running: %q", p.Image)
	}

	// The preview Service must select only preview pods, and must not have
	// inherited the live Service's nodePort.
	var built *corev1.Service
	for i := range k.services {
		if k.services[i].Name == "checkout-api-preview-1234" {
			built = &k.services[i]
		}
	}
	if built == nil {
		t.Fatal("no preview Service was built")
	}
	if built.Spec.Selector[previewLabel] != "checkout-api-preview-1234" || len(built.Spec.Selector) != 1 {
		t.Errorf("the preview Service selects %v", built.Spec.Selector)
	}
	if built.Spec.Ports[0].NodePort != 0 {
		t.Errorf("it copied the live Service's nodePort")
	}
}

// TestApplyRefusesToOverwriteSomebodyElsesObject: the preview name is derived
// from the workload and the work id, so a caller can steer it at an existing
// name. Only objects this tool made are ever written to.
func TestApplyRefusesToOverwriteSomebodyElsesObject(t *testing.T) {
	cfg := cfgFor("shop")
	cfg.readyTimeout = 0
	p := mustValidate(t, cfg, PreviewRequest{
		WorkID: "1234", Workload: "checkout-api", Namespace: "shop",
		Image: "registry.example.com/checkout-api:pr-1234",
	})

	squatter := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "checkout-api-preview-1234", Namespace: "shop",
		Labels: map[string]string{"app": "something-that-was-already-here"},
	}}
	k := &fakeKube{
		deployments: []appsv1.Deployment{*liveDeployment(), squatter},
		services:    []corev1.Service{*liveService()},
	}
	s := serverFor(k, "shop")
	s.cfg = cfg

	err := s.createWorkload(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "not created by agentic-preview") {
		t.Fatalf("want a refusal to touch an object it did not create, got %v", err)
	}
	if len(k.updated) != 0 {
		t.Errorf("it wrote to %v", k.updated)
	}
}

// TestImageIsTakenVerbatim is the boundary the captain drew: this tool knows
// nothing about how an image came to exist or what its tag means. Whatever the
// caller passes is what runs.
func TestImageIsTakenVerbatim(t *testing.T) {
	cfg := cfgFor("shop")
	for _, image := range []string{
		"registry.example.com/checkout-api:pr-1234",
		"some-other-registry.example.net/team/checkout-api:blue-widget",
		"checkout-api@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"quay.example.org:5000/x/y:1.2.3",
	} {
		p := mustValidate(t, cfg, PreviewRequest{
			WorkID: "1234", Workload: "checkout-api", Namespace: "shop", Image: image,
		})
		d, _, err := buildPreviewDeployment(liveDeployment(), p.Create, p)
		if err != nil {
			t.Fatalf("%s: %v", image, err)
		}
		if got := d.Spec.Template.Spec.Containers[0].Image; got != image {
			t.Errorf("image %q was rewritten to %q - nothing may be inferred from a reference", image, got)
		}
	}
}

// The request fields that only mean something when a preview is being built
// must be refused rather than quietly ignored, and the ones that mean something
// only when it is not must still be required.
func TestCreateAndRouteOnlyModesAreKeptApart(t *testing.T) {
	cfg := cfgFor("shop", "previews")
	cases := []struct {
		name    string
		req     PreviewRequest
		wantErr string
	}{{
		name: "REFUSED: previewService alongside image",
		req: PreviewRequest{WorkID: "1234", Workload: "checkout-api", Namespace: "shop",
			Image: "registry.example.com/checkout-api:pr-1234", PreviewService: "something-i-deployed"},
		wantErr: "previewService cannot be given alongside image",
	}, {
		name: "REFUSED: building into a different namespace from the workload",
		req: PreviewRequest{WorkID: "1234", Workload: "checkout-api", Namespace: "shop",
			Image: "registry.example.com/checkout-api:pr-1234", PreviewNamespace: "previews"},
		wantErr: "cannot differ from namespace",
	}, {
		name:    "REFUSED: neither an image nor a Service to route to",
		req:     PreviewRequest{WorkID: "1234", Workload: "checkout-api", Namespace: "shop"},
		wantErr: "previewService is required when nothing is being built",
	}, {
		name: "REFUSED: more replicas than a preview has any business having",
		req: PreviewRequest{WorkID: "1234", Workload: "checkout-api", Namespace: "shop",
			Image: "registry.example.com/checkout-api:pr-1234", Replicas: 500},
		wantErr: "replicas must be between",
	}, {
		name: "ACCEPTED: routing only, exactly as before",
		req: PreviewRequest{WorkID: "1234", Workload: "checkout-api", Namespace: "shop",
			PreviewService: "checkout-api-preview"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := c.req.validate(cfg)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("refused a legitimate request: %v", err)
				}
				if p.Create != nil {
					t.Fatalf("it decided to build something for a routing-only request")
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted a request that must be refused")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("wrong refusal\n  want substring: %s\n  got:            %v", c.wantErr, err)
			}
		})
	}
}

// TestExpiryIsASafetyNetAndIsExtendedByUse pins the teardown policy: teardown
// is explicit, and the only thing that removes a preview on its own is a
// lifetime nothing has touched. Nothing here knows or cares whether a pull
// request merged.
func TestExpiryIsASafetyNetAndIsExtendedByUse(t *testing.T) {
	reg := newRegistry()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	lifetime := 24 * time.Hour

	for _, workload := range []string{"checkout-api", "pricing-api"} {
		if err := reg.add(&Preview{WorkID: "1234", Workload: workload, Namespace: "shop",
			Name: workload + "-1234", ExpiresAt: now.Add(lifetime)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.add(&Preview{WorkID: "blue-widget", Workload: "checkout-api", Namespace: "shop",
		Name: "checkout-api-blue-widget", ExpiresAt: now.Add(lifetime)}); err != nil {
		t.Fatal(err)
	}

	if got := reg.expired(now.Add(23 * time.Hour)); len(got) != 0 {
		t.Fatalf("swept a preview that is still in date: %v", got)
	}

	// Adding the second repository's service to a work id is use of the whole
	// id: both services get the fresh deadline.
	later := now.Add(20 * time.Hour)
	reg.touch("1234", later.Add(lifetime))

	if got := reg.expired(now.Add(25 * time.Hour)); len(got) != 1 || got[0] != "blue-widget" {
		t.Fatalf("want only the untouched work id swept, got %v", got)
	}

	// And the deadline is visible before it arrives, per work id and per
	// service, so it can be extended rather than discovered.
	for _, w := range reg.works() {
		if w.ExpiresAt.IsZero() {
			t.Errorf("work id %s does not report when it expires", w.WorkID)
		}
		for _, svc := range w.Services {
			if svc.ExpiresAt.IsZero() {
				t.Errorf("%s.%s does not report when it expires", svc.Workload, svc.Namespace)
			}
		}
	}
	for _, w := range reg.works() {
		if w.WorkID == "1234" && !w.ExpiresAt.Equal(later.Add(lifetime)) {
			t.Errorf("a work id reports the wrong deadline: %s", w.ExpiresAt)
		}
	}
}
