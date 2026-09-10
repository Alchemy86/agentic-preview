package main

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// kubeAPI is the ENTIRE Kubernetes surface agentic-preview uses. It is an
// interface rather than a *kubernetes.Clientset for two reasons, and the second
// is the important one:
//
//   - the preview builder and the label-scoped delete are the pieces most worth
//     testing, and neither should need a cluster to run against;
//   - it is a written-down inventory of what this service asks Kubernetes for.
//     The RBAC in deploy/rbac.yaml has to grant exactly this and nothing more,
//     and a permission that is not in this interface is a permission the code
//     cannot use. Adding a method here means adding a verb there.
type kubeAPI interface {
	getDeployment(ctx context.Context, ns, name string) (*appsv1.Deployment, error)
	createDeployment(ctx context.Context, d *appsv1.Deployment) (*appsv1.Deployment, error)
	updateDeployment(ctx context.Context, d *appsv1.Deployment) (*appsv1.Deployment, error)
	listDeployments(ctx context.Context, ns, selector string) ([]appsv1.Deployment, error)
	deleteDeployment(ctx context.Context, ns, name string) error

	getService(ctx context.Context, ns, name string) (*corev1.Service, error)
	createService(ctx context.Context, svc *corev1.Service) (*corev1.Service, error)
	updateService(ctx context.Context, svc *corev1.Service) (*corev1.Service, error)
	listServices(ctx context.Context, ns, selector string) ([]corev1.Service, error)
	deleteService(ctx context.Context, ns, name string) error

	listPods(ctx context.Context, ns, selector string) ([]corev1.Pod, error)
}

// clusterKube is the real implementation, against the in-cluster API server.
type clusterKube struct{ cs kubernetes.Interface }

// newKubeAPI builds a client from the pod's own ServiceAccount. It returns an
// error rather than exiting when there is no in-cluster config: creating a
// preview is one half of what this service does and routing to an
// already-deployed Service is the other, so a process that cannot reach the
// Kubernetes API is still useful and says so per request instead of refusing to
// start.
func newKubeAPI() (kubeAPI, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("no in-cluster Kubernetes config: %w", err)
	}
	// A preview create is a handful of calls behind one HTTP request; the
	// client-go defaults (5 qps) would throttle a pipeline raising several
	// services at once for no reason.
	cfg.QPS, cfg.Burst = 20, 40
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building Kubernetes client: %w", err)
	}
	return &clusterKube{cs: cs}, nil
}

func (k *clusterKube) getDeployment(ctx context.Context, ns, name string) (*appsv1.Deployment, error) {
	return k.cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
}

func (k *clusterKube) createDeployment(ctx context.Context, d *appsv1.Deployment) (*appsv1.Deployment, error) {
	return k.cs.AppsV1().Deployments(d.Namespace).Create(ctx, d, metav1.CreateOptions{})
}

func (k *clusterKube) updateDeployment(ctx context.Context, d *appsv1.Deployment) (*appsv1.Deployment, error) {
	return k.cs.AppsV1().Deployments(d.Namespace).Update(ctx, d, metav1.UpdateOptions{})
}

func (k *clusterKube) listDeployments(ctx context.Context, ns, selector string) ([]appsv1.Deployment, error) {
	l, err := k.cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (k *clusterKube) deleteDeployment(ctx context.Context, ns, name string) error {
	err := k.cs.AppsV1().Deployments(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (k *clusterKube) getService(ctx context.Context, ns, name string) (*corev1.Service, error) {
	return k.cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
}

func (k *clusterKube) createService(ctx context.Context, svc *corev1.Service) (*corev1.Service, error) {
	return k.cs.CoreV1().Services(svc.Namespace).Create(ctx, svc, metav1.CreateOptions{})
}

func (k *clusterKube) updateService(ctx context.Context, svc *corev1.Service) (*corev1.Service, error) {
	return k.cs.CoreV1().Services(svc.Namespace).Update(ctx, svc, metav1.UpdateOptions{})
}

func (k *clusterKube) listServices(ctx context.Context, ns, selector string) ([]corev1.Service, error) {
	l, err := k.cs.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (k *clusterKube) deleteService(ctx context.Context, ns, name string) error {
	err := k.cs.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (k *clusterKube) listPods(ctx context.Context, ns, selector string) ([]corev1.Pod, error) {
	l, err := k.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

func isNotFound(err error) bool { return apierrors.IsNotFound(err) }
