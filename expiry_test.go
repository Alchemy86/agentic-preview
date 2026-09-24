package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The executable form of the two things expiry has to get right, both of which
// were wrong on a live cluster: it has to SEE previews this process did not
// raise, and it must not drop a header route to something that is still up.

func expiryServer(k kubeAPI, namespaces ...string) *Server {
	s := serverFor(k, namespaces...)
	s.cfg.lifetime = 48 * time.Hour
	s.cfg.reapRecheck = 12 * time.Hour
	// Short, so the tests that deliberately cannot confirm a teardown do not
	// spend the real confirmation window proving it.
	s.cfg.reapConfirm = 200 * time.Millisecond
	s.orphanSeen = map[orphanKey]bool{}
	s.orphanNextTry = map[orphanKey]time.Time{}
	s.orphanQuiet = map[string]time.Time{}
	return s
}

// previewObjects is one preview's Deployment and Service, labelled exactly as
// createWorkload labels them, of a given age.
func previewObjects(ns, workID, workload, name string, age time.Duration) (appsv1.Deployment, corev1.Service) {
	p := &Preview{WorkID: workID, Workload: workload, Namespace: ns}
	meta := metav1.ObjectMeta{
		Namespace:         ns,
		Name:              name,
		Labels:            previewLabels(p, name),
		CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
	}
	return appsv1.Deployment{ObjectMeta: meta}, corev1.Service{ObjectMeta: meta}
}

func registered(s *Server, ns, workID, workload, name string) *Preview {
	p := &Preview{
		WorkID: workID, Workload: workload, Namespace: ns,
		Name:      workload + "-" + workID,
		ExpiresAt: time.Now().Add(-time.Minute),
		Created:   &CreatedObjects{Namespace: ns, Deployment: name, Service: name},
	}
	if err := s.reg.add(p); err != nil {
		panic(err)
	}
	return p
}

// TestExpiryRemovesTheObjectsBeforeTheHeaderRoute is the order that matters.
// A DELETE may drop the route first - somebody said they were finished. A
// timer may not: the route is what a wrong guess costs, so the objects go
// first and the route only once they are confirmed gone.
func TestExpiryRemovesTheObjectsBeforeTheHeaderRoute(t *testing.T) {
	d, svc := previewObjects("shop", "1234", "checkout-api", "checkout-api-preview-1234", 50*time.Hour)
	k := &fakeKube{deployments: []appsv1.Deployment{d}, services: []corev1.Service{svc}}
	s := expiryServer(k, "shop")
	registered(s, "shop", "1234", "checkout-api", "checkout-api-preview-1234")

	if got := s.reg.expired(time.Now()); len(got) != 1 || got[0] != "1234" {
		t.Fatalf("the sweep did not consider it expired: %v", got)
	}
	s.expireWork(context.Background(), "1234")

	if len(k.deletedDeployments) != 1 || len(k.deletedServices) != 1 {
		t.Fatalf("it did not delete both objects: deployments %v services %v",
			k.deletedDeployments, k.deletedServices)
	}
	if _, still := s.reg.get(serviceKey{WorkID: "1234", Namespace: "shop", Workload: "checkout-api"}); still {
		t.Fatal("the objects are gone and the header route is still registered")
	}
}

// stuckKube accepts a delete and changes nothing, which is what a wedged
// finalizer or a revoked delete permission looks like from here.
type stuckKube struct{ *fakeKube }

func (b stuckKube) deleteDeployment(_ context.Context, ns, name string) error {
	b.fakeKube.deletedDeployments = append(b.fakeKube.deletedDeployments, name+"."+ns)
	return nil
}

func (b stuckKube) deleteService(_ context.Context, ns, name string) error {
	b.fakeKube.deletedServices = append(b.fakeKube.deletedServices, name+"."+ns)
	return nil
}

// TestExpiryKeepsTheRouteWhenTeardownCannotBeConfirmed is the failure this
// ordering exists for. Removing the intercept from a preview whose pod is
// still running does not fall back to the live workload - the traffic-agent
// holds the header on an unbounded retry and it answers nothing at all. So an
// unconfirmed teardown keeps the route and moves the deadline out instead.
func TestExpiryKeepsTheRouteWhenTeardownCannotBeConfirmed(t *testing.T) {
	d, svc := previewObjects("shop", "1234", "checkout-api", "checkout-api-preview-1234", 50*time.Hour)
	k := stuckKube{&fakeKube{deployments: []appsv1.Deployment{d}, services: []corev1.Service{svc}}}
	s := expiryServer(k, "shop")
	s.cfg.reapRecheck = 12 * time.Hour
	registered(s, "shop", "1234", "checkout-api", "checkout-api-preview-1234")

	start := time.Now()
	s.expireWork(context.Background(), "1234")

	p, still := s.reg.get(serviceKey{WorkID: "1234", Namespace: "shop", Workload: "checkout-api"})
	if !still {
		t.Fatal("it dropped the header route while the preview was still running")
	}
	if wait := p.ExpiresAt.Sub(start); wait < 11*time.Hour || wait > 13*time.Hour {
		t.Fatalf("the deadline did not move out by the recheck interval: %s", wait)
	}
	if len(k.fakeKube.deletedDeployments) == 0 {
		t.Fatal("it did not even try to delete the objects")
	}
}

// blindKube cannot list one namespace at all, which is the shipped cluster's
// actual state whenever an RBAC Role lags ALLOWED_NAMESPACES.
type blindKube struct {
	*fakeKube
	blind string
}

func (b blindKube) listDeployments(ctx context.Context, ns, selector string) ([]appsv1.Deployment, error) {
	if ns == b.blind {
		return nil, forbiddenIn(ns)
	}
	return b.fakeKube.listDeployments(ctx, ns, selector)
}

func (b blindKube) listServices(ctx context.Context, ns, selector string) ([]corev1.Service, error) {
	if ns == b.blind {
		return nil, forbiddenIn(ns)
	}
	return b.fakeKube.listServices(ctx, ns, selector)
}

func forbiddenIn(ns string) error {
	return fmt.Errorf("is forbidden: no permission to list in %s", ns)
}

// TestExpiryIsNotStalledByAnUnrelatedNamespace pins the scope of the
// confirmation. dropWorkload sweeps every allowed namespace and reports one it
// cannot read as a problem; that is a standing condition on a cluster whose
// RBAC has not caught up, and if it counted as "cannot confirm" then nothing
// would ever expire again. Whether a work id is gone is answered in the
// namespaces it was actually raised in.
func TestExpiryIsNotStalledByAnUnrelatedNamespace(t *testing.T) {
	d, svc := previewObjects("shop", "1234", "checkout-api", "checkout-api-preview-1234", 50*time.Hour)
	k := blindKube{&fakeKube{deployments: []appsv1.Deployment{d}, services: []corev1.Service{svc}}, "vault"}
	s := expiryServer(k, "shop", "vault")
	registered(s, "shop", "1234", "checkout-api", "checkout-api-preview-1234")

	s.expireWork(context.Background(), "1234")

	if _, still := s.reg.get(serviceKey{WorkID: "1234", Namespace: "shop", Workload: "checkout-api"}); still {
		t.Fatal("a namespace this work id was never in stalled its expiry")
	}
}

// TestOrphanSweepRemovesAPreviewNothingRemembers is the measured fault: the
// registry is in memory, so a preview raised before this process started has
// no deadline anywhere and lives forever. Its objects still carry their own
// labels and creation time, and that is what the sweep reads.
func TestOrphanSweepRemovesAPreviewNothingRemembers(t *testing.T) {
	d, svc := previewObjects("shop", "pr-190", "search-api", "search-api-preview-pr-190", 310*time.Hour)
	k := &fakeKube{deployments: []appsv1.Deployment{d}, services: []corev1.Service{svc}}
	s := expiryServer(k, "shop")

	// One sighting is never enough: the registry is empty for a moment at
	// startup and the schedule controller fills it asynchronously.
	s.sweepOrphans(context.Background())
	if len(k.deletedDeployments) != 0 {
		t.Fatalf("it removed an orphan on a single sighting: %v", k.deletedDeployments)
	}

	s.sweepOrphans(context.Background())
	if len(k.deletedDeployments) != 1 || len(k.deletedServices) != 1 {
		t.Fatalf("the orphan is still up: deployments %v services %v", k.deletedDeployments, k.deletedServices)
	}
	if !strings.Contains(k.deletedDeployments[0], "search-api-preview-pr-190") {
		t.Fatalf("it deleted the wrong thing: %v", k.deletedDeployments)
	}
}

// TestOrphanSweepLeavesLivePreviewsAlone is the other half, and the one that
// keeps the sweep from being a second, dumber expiry policy. A preview the
// registry knows about is owned by its deadline whatever its age - it may have
// been extended minutes ago - and a preview younger than the lifetime is not
// an orphan even when nothing remembers it.
func TestOrphanSweepLeavesLivePreviewsAlone(t *testing.T) {
	old, oldSvc := previewObjects("shop", "1234", "checkout-api", "checkout-api-preview-1234", 300*time.Hour)
	young, youngSvc := previewObjects("shop", "5678", "basket-api", "basket-api-preview-5678", time.Hour)
	k := &fakeKube{
		deployments: []appsv1.Deployment{old, young},
		services:    []corev1.Service{oldSvc, youngSvc},
	}
	s := expiryServer(k, "shop")

	// Tracked, and deliberately long past the lifetime: its deadline owns it.
	p := registered(s, "shop", "1234", "checkout-api", "checkout-api-preview-1234")
	p.ExpiresAt = time.Now().Add(time.Hour)

	s.sweepOrphans(context.Background())
	s.sweepOrphans(context.Background())

	if len(k.deletedDeployments) != 0 || len(k.deletedServices) != 0 {
		t.Fatalf("the sweep took a preview that was not an orphan: deployments %v services %v",
			k.deletedDeployments, k.deletedServices)
	}
}

// TestOrphanSweepIgnoresWhatItDidNotMake. The sweep lists on the managed-by
// label, but an object carrying that label without the work-id and workload
// labels a teardown selects on cannot be named safely, so it is left alone
// rather than guessed at.
func TestOrphanSweepIgnoresWhatItDidNotMake(t *testing.T) {
	d, svc := previewObjects("shop", "1234", "checkout-api", "checkout-api-preview-1234", 300*time.Hour)
	delete(d.Labels, workIDLabel)
	delete(svc.Labels, workIDLabel)
	k := &fakeKube{deployments: []appsv1.Deployment{d}, services: []corev1.Service{svc}}
	s := expiryServer(k, "shop")

	s.sweepOrphans(context.Background())
	s.sweepOrphans(context.Background())

	if len(k.deletedDeployments) != 0 || len(k.deletedServices) != 0 {
		t.Fatalf("it deleted an object it could not name: %v %v", k.deletedDeployments, k.deletedServices)
	}
}

// TestOrphanSweepConfirmsOneWorkloadNotTheWholeWorkID. Three services of one
// work id go stale together and arrive as three orphans. Each one confirms its
// own objects: confirming on the work id would have the first report its two
// siblings as still there and sit on a 12h recheck over its own success.
func TestOrphanSweepConfirmsOneWorkloadNotTheWholeWorkID(t *testing.T) {
	var deps []appsv1.Deployment
	var svcs []corev1.Service
	for _, w := range []string{"checkout-api", "basket-api", "search-api"} {
		d, svc := previewObjects("shop", "pr3304", w, w+"-preview-pr3304", 200*time.Hour)
		deps = append(deps, d)
		svcs = append(svcs, svc)
	}
	k := &fakeKube{deployments: deps, services: svcs}
	s := expiryServer(k, "shop")

	s.sweepOrphans(context.Background())
	s.sweepOrphans(context.Background())

	if len(k.deletedDeployments) != 3 || len(k.deletedServices) != 3 {
		t.Fatalf("a sibling under the same work id blocked a teardown: deployments %v services %v",
			k.deletedDeployments, k.deletedServices)
	}
	if len(s.orphanNextTry) != 0 {
		t.Fatalf("a confirmed teardown was recorded as needing a recheck: %v", s.orphanNextTry)
	}
}
