package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newRegistryCertificate() *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(registryCertificateName)
	cert.SetNamespace(registryNamespace)
	return cert
}

func createRegistryCertificate(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	cert := newRegistryCertificate()
	if err := unstructured.SetNestedField(cert.Object, registryCertificateName, "spec", "secretName"); err != nil {
		t.Fatalf("failed to set Certificate spec: %v", err)
	}
	if err := k8sClient.Create(ctx, cert); err != nil {
		t.Fatalf("failed to create registry Certificate: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), newRegistryCertificate()) })
}

func setRegistryCertRevision(t *testing.T, revision int64) {
	t.Helper()
	eventually(t, "registry Certificate revision update", func(ctx context.Context) (bool, error) {
		cert := newRegistryCertificate()
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: registryNamespace, Name: registryCertificateName}, cert); err != nil {
			return false, err
		}
		if err := unstructured.SetNestedField(cert.Object, revision, "status", "revision"); err != nil {
			return false, err
		}
		if err := k8sClient.Status().Update(ctx, cert); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

func createRegistryDeployment(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	labels := map[string]string{"app.kubernetes.io/name": registryDeploymentName}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: registryDeploymentName, Namespace: registryNamespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "registry", Image: "registry:test"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, deploy); err != nil {
		t.Fatalf("failed to create registry Deployment: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), deploy) })
}

func getRegistryDeployment(t *testing.T, ctx context.Context) *appsv1.Deployment {
	t.Helper()
	var deploy appsv1.Deployment
	key := types.NamespacedName{Namespace: registryNamespace, Name: registryDeploymentName}
	if err := k8sClient.Get(ctx, key, &deploy); err != nil {
		t.Fatalf("failed to get registry Deployment: %v", err)
	}
	return &deploy
}

func waitForRegistryCertAnnotation(t *testing.T, want string) {
	t.Helper()
	key := types.NamespacedName{Namespace: registryNamespace, Name: registryDeploymentName}
	eventually(t, "registry Deployment cert-revision annotation "+want, func(ctx context.Context) (bool, error) {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			return false, err
		}
		return deploy.Spec.Template.Annotations[registryCertRevisionAnnotation] == want, nil
	})
}

func TestRegistryCertRotationRollsDeployment(t *testing.T) {
	ctx := context.Background()

	createRegistryDeployment(t)
	createRegistryCertificate(t)

	// Before any issuance there is no revision and nothing to roll onto: the
	// pod that comes up will mount whatever the first issuance writes.
	time.Sleep(1500 * time.Millisecond)
	if ann := getRegistryDeployment(t, ctx).Spec.Template.Annotations[registryCertRevisionAnnotation]; ann != "" {
		t.Fatalf("Deployment annotated %q before the Certificate had a revision", ann)
	}

	setRegistryCertRevision(t, 1)
	waitForRegistryCertAnnotation(t, "1")
	generationAfterFirst := getRegistryDeployment(t, ctx).Generation

	// Idempotency: once the annotation matches the revision, further reconciles
	// (the controller re-runs on every Certificate/Deployment event) must not
	// keep rewriting the template. A one-character inversion of the equality
	// guard would roll the pod forever; pin that it does not.
	resourceVersionAtRest := getRegistryDeployment(t, ctx).ResourceVersion
	time.Sleep(1500 * time.Millisecond)
	if rv := getRegistryDeployment(t, ctx).ResourceVersion; rv != resourceVersionAtRest {
		t.Fatalf("registry Deployment kept updating at a steady revision: resourceVersion %s -> %s", resourceVersionAtRest, rv)
	}

	// Renewal: cert-manager bumps status.revision; the template annotation
	// must follow, and the template change must bump the Deployment
	// generation — that is the rollout.
	setRegistryCertRevision(t, 2)
	waitForRegistryCertAnnotation(t, "2")
	if gen := getRegistryDeployment(t, ctx).Generation; gen <= generationAfterFirst {
		t.Fatalf("Deployment generation did not advance on renewal: %d -> %d", generationAfterFirst, gen)
	}

	// Drift heal: a hand-stripped annotation converges back without a
	// Certificate event (the Deployment watch).
	eventually(t, "annotation strip to apply", func(ctx context.Context) (bool, error) {
		deploy := getRegistryDeployment(t, ctx)
		delete(deploy.Spec.Template.Annotations, registryCertRevisionAnnotation)
		if err := k8sClient.Update(ctx, deploy); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	waitForRegistryCertAnnotation(t, "2")

	// Adoption: a Deployment created after the Certificate already has a
	// revision (operator upgrade, re-install) converges without any
	// Certificate event.
	deploy := getRegistryDeployment(t, ctx)
	if err := k8sClient.Delete(ctx, deploy); err != nil {
		t.Fatalf("failed to delete registry Deployment: %v", err)
	}
	eventually(t, "registry Deployment to be gone", func(ctx context.Context) (bool, error) {
		var d appsv1.Deployment
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: registryNamespace, Name: registryDeploymentName}, &d)
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
	createRegistryDeployment(t)
	waitForRegistryCertAnnotation(t, "2")
}
