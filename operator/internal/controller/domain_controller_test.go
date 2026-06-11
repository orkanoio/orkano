package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

const testClusterIssuer = "test-cluster-issuer"

func createDomain(t *testing.T, name, host, appName string) *orkanov1alpha1.Domain {
	t.Helper()
	ctx := context.Background()
	domain := &orkanov1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec: orkanov1alpha1.DomainSpec{
			Host:   host,
			AppRef: orkanov1alpha1.LocalObjectRef{Name: appName},
		},
	}
	if err := k8sClient.Create(ctx, domain); err != nil {
		t.Fatalf("failed to create Domain %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, domain) })
	return domain
}

func getIngress(t *testing.T, name string) *networkingv1.Ingress {
	t.Helper()
	var ing networkingv1.Ingress
	key := types.NamespacedName{Name: name, Namespace: appsNamespace}
	eventually(t, "Ingress "+name, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, key, &ing); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	return &ing
}

// newCertificate plays the part of cert-manager's ingress-shim, which names
// the Certificate after the Ingress TLS secret.
func newCertificate(name string) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	cert.SetName(name)
	cert.SetNamespace(appsNamespace)
	return cert
}

func createCertificate(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	cert := newCertificate(name)
	if err := unstructured.SetNestedField(cert.Object, name, "spec", "secretName"); err != nil {
		t.Fatalf("failed to set Certificate spec: %v", err)
	}
	if err := k8sClient.Create(ctx, cert); err != nil {
		t.Fatalf("failed to create Certificate %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, newCertificate(name)) })
}

func setCertificateReady(t *testing.T, name string, status metav1.ConditionStatus, reason, message string) {
	t.Helper()
	eventually(t, "Certificate status update for "+name, func(ctx context.Context) (bool, error) {
		cert := newCertificate(name)
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appsNamespace}, cert); err != nil {
			return false, err
		}
		conditions := []any{map[string]any{
			"type":    "Ready",
			"status":  string(status),
			"reason":  reason,
			"message": message,
		}}
		if err := unstructured.SetNestedSlice(cert.Object, conditions, "status", "conditions"); err != nil {
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

func waitForDomainCondition(t *testing.T, name, condType string, status metav1.ConditionStatus, reason string) *orkanov1alpha1.Domain {
	t.Helper()
	var domain orkanov1alpha1.Domain
	eventually(t, fmt.Sprintf("Domain %s %s=%s/%s", name, condType, status, reason), func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appsNamespace}, &domain); err != nil {
			return false, err
		}
		cond := meta.FindStatusCondition(domain.Status.Conditions, condType)
		return cond != nil && cond.Status == status && cond.Reason == reason &&
			domain.Status.ObservedGeneration == domain.Generation, nil
	})
	return &domain
}

func waitForAppURL(t *testing.T, appName, url string) {
	t.Helper()
	eventually(t, fmt.Sprintf("App %s status.url == %q", appName, url), func(ctx context.Context) (bool, error) {
		var app orkanov1alpha1.App
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: appName, Namespace: appsNamespace}, &app); err != nil {
			return false, err
		}
		return app.Status.URL == url, nil
	})
}

func TestDomainRendersIngressAndTracksCertificate(t *testing.T) {
	ctx := context.Background()
	app := createApp(t, "dom-web", nil)
	domain := createDomain(t, "dom-web-example", "dom-web.example.com", app.Name)

	ing := getIngress(t, domain.Name)
	owner := metav1.GetControllerOf(ing)
	if owner == nil || owner.Kind != "Domain" || owner.Name != domain.Name {
		t.Fatalf("Ingress not controller-owned by the Domain: %+v", owner)
	}
	if got := ing.Annotations[clusterIssuerAnnotation]; got != testClusterIssuer {
		t.Errorf("issuer annotation = %q, want %q", got, testClusterIssuer)
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != domain.Spec.Host {
		t.Fatalf("rules = %+v, want one rule for %s", ing.Spec.Rules, domain.Spec.Host)
	}
	paths := ing.Spec.Rules[0].HTTP.Paths
	if len(paths) != 1 || paths[0].Path != "/" || *paths[0].PathType != networkingv1.PathTypePrefix {
		t.Errorf("paths = %+v, want a single / Prefix path", paths)
	}
	backend := paths[0].Backend.Service
	if backend == nil || backend.Name != app.Name || backend.Port.Name != servicePortName {
		t.Errorf("backend = %+v, want Service %s port %q", backend, app.Name, servicePortName)
	}
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != domain.Name+tlsSecretSuffix ||
		len(ing.Spec.TLS[0].Hosts) != 1 || ing.Spec.TLS[0].Hosts[0] != domain.Spec.Host {
		t.Errorf("tls = %+v, want host %s with secret %s", ing.Spec.TLS, domain.Spec.Host, domain.Name+tlsSecretSuffix)
	}
	if ing.Spec.IngressClassName != nil {
		t.Errorf("ingressClassName = %v, want unset (cluster default)", *ing.Spec.IngressClassName)
	}

	// No Certificate yet: ingress-shim has not acted.
	waitForDomainCondition(t, domain.Name, orkanov1alpha1.ConditionCertificateReady, metav1.ConditionUnknown, reasonCertificatePending)
	waitForDomainCondition(t, domain.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonCertificatePending)

	certName := domain.Name + tlsSecretSuffix
	createCertificate(t, certName)
	setCertificateReady(t, certName, metav1.ConditionFalse, "DoesNotExist", "issuing")
	waitForDomainCondition(t, domain.Name, orkanov1alpha1.ConditionCertificateReady, metav1.ConditionFalse, "DoesNotExist")
	waitForDomainCondition(t, domain.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonCertificatePending)

	setCertificateReady(t, certName, metav1.ConditionTrue, "Ready", "certificate issued")
	waitForDomainCondition(t, domain.Name, orkanov1alpha1.ConditionCertificateReady, metav1.ConditionTrue, "Ready")
	waitForDomainCondition(t, domain.Name, orkanov1alpha1.ConditionReady, metav1.ConditionTrue, reasonAvailable)
	waitForAppURL(t, app.Name, "https://dom-web.example.com")

	// The mutate closure must be a no-op against the stored object, or every
	// reconcile produces a spurious update.
	r := &DomainReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), ClusterIssuer: testClusterIssuer}
	var fresh orkanov1alpha1.Domain
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: domain.Name, Namespace: appsNamespace}, &fresh); err != nil {
		t.Fatalf("failed to refetch Domain: %v", err)
	}
	var stored networkingv1.Ingress
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: domain.Name, Namespace: appsNamespace}, &stored); err != nil {
		t.Fatalf("failed to refetch Ingress: %v", err)
	}
	before := stored.DeepCopy()
	r.mutateIngress(&fresh, &stored)
	if err := controllerutil.SetControllerReference(&fresh, &stored, r.Scheme); err != nil {
		t.Fatalf("failed to set controller reference: %v", err)
	}
	if !equality.Semantic.DeepEqual(before, &stored) {
		t.Errorf("Ingress mutate closure is not stable against the stored object:\nbefore: %+v\nafter:  %+v", before, &stored)
	}
}

func TestDomainAppNotFoundHealsWhenAppAppears(t *testing.T) {
	domain := createDomain(t, "dom-orphan", "orphan.example.com", "dom-orphan-app")

	// The Ingress renders regardless, so cert issuance starts in parallel
	// with the App's first build instead of after it.
	getIngress(t, domain.Name)
	waitForDomainCondition(t, domain.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonAppNotFound)

	createApp(t, "dom-orphan-app", nil)
	waitForDomainCondition(t, domain.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonCertificatePending)
}

func TestDomainWorkerAppNotRoutable(t *testing.T) {
	app := createApp(t, "dom-worker", func(a *orkanov1alpha1.App) {
		a.Spec.Type = orkanov1alpha1.WorkloadWorker
	})
	domain := createDomain(t, "dom-worker-example", "worker.example.com", app.Name)

	waitForDomainCondition(t, domain.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonAppNotRoutable)

	time.Sleep(1500 * time.Millisecond)
	var fresh orkanov1alpha1.App
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &fresh); err != nil {
		t.Fatalf("failed to refetch App: %v", err)
	}
	if fresh.Status.URL != "" {
		t.Errorf("status.url = %q on a Worker app, want empty", fresh.Status.URL)
	}
}

func TestDomainHostConflictOldestWins(t *testing.T) {
	ctx := context.Background()
	app := createApp(t, "dom-conflict", nil)

	// Sub-second creations tie on creationTimestamp; names break the tie the
	// same direction, so "a" beats "b" under either clock outcome.
	winner := createDomain(t, "dom-conflict-a", "conflict.example.com", app.Name)
	getIngress(t, winner.Name)
	loser := createDomain(t, "dom-conflict-b", "conflict.example.com", app.Name)

	waitForDomainCondition(t, loser.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonHostConflict)
	cond := meta.FindStatusCondition(
		waitForDomainCondition(t, loser.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonHostConflict).Status.Conditions,
		orkanov1alpha1.ConditionCertificateReady)
	if cond != nil {
		t.Errorf("loser carries CertificateReady = %+v, want none", cond)
	}

	time.Sleep(1500 * time.Millisecond)
	var ing networkingv1.Ingress
	err := k8sClient.Get(ctx, types.NamespacedName{Name: loser.Name, Namespace: appsNamespace}, &ing)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no Ingress for the conflict loser, got: %v", err)
	}
	waitForDomainCondition(t, winner.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonCertificatePending)

	// Deleting the winner promotes the loser without polling.
	if err := k8sClient.Delete(ctx, winner); err != nil {
		t.Fatalf("failed to delete winning Domain: %v", err)
	}
	getIngress(t, loser.Name)
	waitForDomainCondition(t, loser.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonCertificatePending)
}

func TestDomainURLFollowsOldestAndClearsOnDelete(t *testing.T) {
	ctx := context.Background()
	app := createApp(t, "dom-url", nil)

	// Named so the tie-break favors the first-created Domain even when both
	// land in the same creationTimestamp second.
	first := createDomain(t, "dom-url-a", "url-a.example.com", app.Name)
	waitForAppURL(t, app.Name, "https://url-a.example.com")
	second := createDomain(t, "dom-url-b", "url-b.example.com", app.Name)
	// Barrier: Reconcile derives the URL before writing the condition, so
	// once the condition exists the younger Domain has had its chance to
	// (wrongly) displace the URL — only then is the assertion non-vacuous.
	waitForDomainCondition(t, second.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonCertificatePending)
	var fresh orkanov1alpha1.App
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &fresh); err != nil {
		t.Fatalf("failed to refetch App: %v", err)
	}
	if fresh.Status.URL != "https://url-a.example.com" {
		t.Errorf("status.url = %q after younger Domain appeared, want the oldest Domain's host", fresh.Status.URL)
	}

	if err := k8sClient.Delete(ctx, first); err != nil {
		t.Fatalf("failed to delete Domain: %v", err)
	}
	waitForAppURL(t, app.Name, "https://url-b.example.com")

	if err := k8sClient.Delete(ctx, second); err != nil {
		t.Fatalf("failed to delete Domain: %v", err)
	}
	waitForAppURL(t, app.Name, "")

	eventually(t, "deleted Domains to be finalized and gone", func(ctx context.Context) (bool, error) {
		var d orkanov1alpha1.Domain
		err := k8sClient.Get(ctx, types.NamespacedName{Name: first.Name, Namespace: appsNamespace}, &d)
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: second.Name, Namespace: appsNamespace}, &d)
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
}

func TestDomainAppRefIsImmutable(t *testing.T) {
	app := createApp(t, "dom-repoint", nil)
	createApp(t, "dom-repoint-v2", nil)
	domain := createDomain(t, "dom-repoint-example", "repoint.example.com", app.Name)

	// Re-pointing would strand the old App's derived status.url with no
	// event left to heal it; the schema forces delete-and-recreate instead.
	// Retried because the controller's finalizer write races this update.
	eventually(t, "appRef re-point to be rejected as invalid", func(pollCtx context.Context) (bool, error) {
		var fresh orkanov1alpha1.Domain
		if err := k8sClient.Get(pollCtx, types.NamespacedName{Name: domain.Name, Namespace: appsNamespace}, &fresh); err != nil {
			return false, err
		}
		fresh.Spec.AppRef.Name = "dom-repoint-v2"
		err := k8sClient.Update(pollCtx, &fresh)
		if apierrors.IsConflict(err) {
			return false, nil
		}
		if !apierrors.IsInvalid(err) {
			return false, fmt.Errorf("expected re-pointing appRef to be rejected, got: %w", err)
		}
		return true, nil
	})
}

func TestDomainHostConflictTimestampBeatsName(t *testing.T) {
	app := createApp(t, "dom-tie", nil)

	// The older Domain gets the lexicographically larger name, so a winner
	// picked by name order (or an inverted timestamp comparison) would
	// promote the wrong Domain. The sleep guarantees the second create
	// lands in a strictly later creationTimestamp second.
	older := createDomain(t, "dom-tie-z", "tie.example.com", app.Name)
	getIngress(t, older.Name)
	time.Sleep(1200 * time.Millisecond)
	younger := createDomain(t, "dom-tie-a", "tie.example.com", app.Name)

	waitForDomainCondition(t, younger.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonHostConflict)
	waitForDomainCondition(t, older.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonCertificatePending)

	var a, z orkanov1alpha1.Domain
	ctx := context.Background()
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: older.Name, Namespace: appsNamespace}, &z); err != nil {
		t.Fatalf("failed to refetch Domain: %v", err)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: younger.Name, Namespace: appsNamespace}, &a); err != nil {
		t.Fatalf("failed to refetch Domain: %v", err)
	}
	if !z.CreationTimestamp.Before(&a.CreationTimestamp) {
		t.Fatalf("test setup broken: timestamps tie (%s vs %s), the name tie-break would mask the assertion",
			z.CreationTimestamp, a.CreationTimestamp)
	}
}

func TestDomainNeverAdoptsForeignIngress(t *testing.T) {
	ctx := context.Background()
	app := createApp(t, "dom-foreign", nil)

	pathType := networkingv1.PathTypePrefix
	foreign := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "dom-foreign-example", Namespace: appsNamespace},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: "pre-existing.example.com",
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: "someone-elses-service",
									Port: networkingv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
	if err := k8sClient.Create(ctx, foreign); err != nil {
		t.Fatalf("failed to create foreign Ingress: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

	domain := createDomain(t, "dom-foreign-example", "foreign.example.com", app.Name)
	waitForDomainCondition(t, domain.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonReconcileError)

	var got networkingv1.Ingress
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: foreign.Name, Namespace: appsNamespace}, &got); err != nil {
		t.Fatalf("failed to refetch foreign Ingress: %v", err)
	}
	if got.Spec.Rules[0].Host != "pre-existing.example.com" {
		t.Errorf("foreign Ingress host rewritten to %q", got.Spec.Rules[0].Host)
	}
	if metav1.GetControllerOf(&got) != nil {
		t.Errorf("foreign Ingress was adopted: %+v", metav1.GetControllerOf(&got))
	}
}

func TestDomainExampleReconciles(t *testing.T) {
	applyExample(t, "02-web-service-postgres.yaml")

	ing := getIngress(t, "api-example-com")
	if ing.Spec.Rules[0].Host != "api.example.com" {
		t.Errorf("host = %q, want api.example.com", ing.Spec.Rules[0].Host)
	}
	if backend := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service; backend == nil || backend.Name != "api" {
		t.Errorf("backend = %+v, want Service api", backend)
	}
	waitForAppURL(t, "api", "https://api.example.com")
}
