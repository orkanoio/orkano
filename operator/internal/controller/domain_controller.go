package controller

import (
	"context"
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

const (
	domainHostIndex    = "spec.host"
	domainAppNameIndex = "spec.appRef.name"

	// domainFinalizer exists only to recompute App.status.url after a
	// Domain disappears; the Ingress itself is cleaned up by ownerRef GC.
	domainFinalizer = "orkano.io/domain"

	clusterIssuerAnnotation = "cert-manager.io/cluster-issuer"
	tlsSecretSuffix         = "-tls"

	reasonHostConflict       = "HostConflict"
	reasonAppNotFound        = "AppNotFound"
	reasonAppNotRoutable     = "AppNotRoutable"
	reasonCertificatePending = "CertificatePending"
)

// certificateGVK is read and watched as unstructured: pulling in the
// cert-manager Go module for two status fields would add a heavyweight
// dependency the operator only ever reads (ADR-0006: ingress-shim writes).
var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

type DomainReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads around the informer cache where a stale read has no
	// later event to heal it (the finalizer-path URL recompute).
	APIReader client.Reader
	// ClusterIssuer names the platform ClusterIssuer configured at install
	// time; every Domain Ingress is annotated with it (ADR-0006).
	ClusterIssuer string
}

func (r *DomainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var domain orkanov1alpha1.Domain
	if err := r.Get(ctx, req.NamespacedName, &domain); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !domain.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&domain, domainFinalizer) {
			return ctrl.Result{}, nil
		}
		// The dying Domain is excluded from the URL derivation by its own
		// deletionTimestamp, which updateAppURL filters on.
		if err := r.updateAppURL(ctx, domain.Namespace, domain.Spec.AppRef.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("recomputing App URL on Domain deletion: %w", err)
		}
		controllerutil.RemoveFinalizer(&domain, domainFinalizer)
		if err := r.Update(ctx, &domain); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&domain, domainFinalizer) {
		if err := r.Update(ctx, &domain); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	statusBefore := domain.Status.DeepCopy()

	winner, err := r.hostWinner(ctx, domain.Spec.Host)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking host conflicts: %w", err)
	}
	if winner.Namespace != domain.Namespace || winner.Name != domain.Name {
		if err := r.deleteOwnIngress(ctx, &domain); err != nil {
			return ctrl.Result{}, r.failReady(ctx, &domain, statusBefore, reasonReconcileError,
				fmt.Errorf("deleting Ingress of conflicting Domain: %w", err))
		}
		meta.RemoveStatusCondition(&domain.Status.Conditions, orkanov1alpha1.ConditionCertificateReady)
		setDomainReady(&domain, metav1.ConditionFalse, reasonHostConflict,
			fmt.Sprintf("host %s is already claimed by older Domain %s/%s", domain.Spec.Host, winner.Namespace, winner.Name))
		if err := r.updateAppURL(ctx, domain.Namespace, domain.Spec.AppRef.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("recomputing App URL: %w", err)
		}
		return ctrl.Result{}, r.updateStatus(ctx, &domain, statusBefore)
	}

	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: domain.Name, Namespace: domain.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, ing, func() error {
		// Never adopt an Ingress Orkano didn't create: silently rewriting a
		// foreign object's routing is a hijack, not reconciliation.
		if !ing.CreationTimestamp.IsZero() && metav1.GetControllerOf(ing) == nil {
			return fmt.Errorf("existing Ingress %s/%s is not managed by Orkano; rename the Domain or delete the Ingress", ing.Namespace, ing.Name)
		}
		r.mutateIngress(&domain, ing)
		return controllerutil.SetControllerReference(&domain, ing, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, r.failReady(ctx, &domain, statusBefore, reasonReconcileError,
			fmt.Errorf("reconciling Ingress: %w", err))
	}
	if op != controllerutil.OperationResultNone {
		log.Info("reconciled Ingress", "operation", op)
	}

	certReady, certReason, certMessage, err := r.observeCertificate(ctx, &domain)
	if err != nil {
		return ctrl.Result{}, r.failReady(ctx, &domain, statusBefore, reasonReconcileError,
			fmt.Errorf("observing Certificate: %w", err))
	}
	setDomainCondition(&domain, orkanov1alpha1.ConditionCertificateReady, certReady, certReason, certMessage)

	var app orkanov1alpha1.App
	err = r.Get(ctx, types.NamespacedName{Namespace: domain.Namespace, Name: domain.Spec.AppRef.Name}, &app)
	switch {
	case apierrors.IsNotFound(err):
		setDomainReady(&domain, metav1.ConditionFalse, reasonAppNotFound,
			fmt.Sprintf("App %s does not exist", domain.Spec.AppRef.Name))
	case err != nil:
		return ctrl.Result{}, r.failReady(ctx, &domain, statusBefore, reasonReconcileError,
			fmt.Errorf("fetching App %s: %w", domain.Spec.AppRef.Name, err))
	case workloadType(&app) != orkanov1alpha1.WorkloadWeb:
		setDomainReady(&domain, metav1.ConditionFalse, reasonAppNotRoutable,
			fmt.Sprintf("App %s is a %s app and serves no traffic", app.Name, workloadType(&app)))
	case certReady != metav1.ConditionTrue:
		setDomainReady(&domain, metav1.ConditionFalse, reasonCertificatePending,
			fmt.Sprintf("certificate for %s is not ready: %s", domain.Spec.Host, certMessage))
	default:
		setDomainReady(&domain, metav1.ConditionTrue, reasonAvailable,
			fmt.Sprintf("routing https://%s to App %s", domain.Spec.Host, app.Name))
	}

	if err := r.updateAppURL(ctx, domain.Namespace, domain.Spec.AppRef.Name); err != nil {
		return ctrl.Result{}, fmt.Errorf("deriving App URL: %w", err)
	}
	return ctrl.Result{}, r.updateStatus(ctx, &domain, statusBefore)
}

// hostWinner returns the Domain that owns a host: oldest creationTimestamp
// across all namespaces, name tie-break, Domains being deleted excluded so
// a dying winner does not block its successor (ADR-0006).
func (r *DomainReconciler) hostWinner(ctx context.Context, host string) (types.NamespacedName, error) {
	var domains orkanov1alpha1.DomainList
	if err := r.List(ctx, &domains, client.MatchingFields{domainHostIndex: host}); err != nil {
		return types.NamespacedName{}, err
	}
	var winner *orkanov1alpha1.Domain
	for i := range domains.Items {
		d := &domains.Items[i]
		if !d.DeletionTimestamp.IsZero() {
			continue
		}
		if winner == nil || olderDomain(d, winner) {
			winner = d
		}
	}
	if winner == nil {
		return types.NamespacedName{}, nil
	}
	return types.NamespacedName{Namespace: winner.Namespace, Name: winner.Name}, nil
}

func olderDomain(a, b *orkanov1alpha1.Domain) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.Namespace < b.Namespace
}

func (r *DomainReconciler) deleteOwnIngress(ctx context.Context, domain *orkanov1alpha1.Domain) error {
	var ing networkingv1.Ingress
	err := r.Get(ctx, types.NamespacedName{Namespace: domain.Namespace, Name: domain.Name}, &ing)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(&ing, domain) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &ing))
}

// mutateIngress assigns only the fields Orkano owns; ingressClassName is
// left to the cluster default (k3s marks Traefik default) so there is no
// dial to misconfigure.
func (r *DomainReconciler) mutateIngress(domain *orkanov1alpha1.Domain, ing *networkingv1.Ingress) {
	if ing.Labels == nil {
		ing.Labels = map[string]string{}
	}
	ing.Labels[appLabel] = domain.Spec.AppRef.Name
	ing.Labels[managedByLabel] = managedByValue
	if ing.Annotations == nil {
		ing.Annotations = map[string]string{}
	}
	ing.Annotations[clusterIssuerAnnotation] = r.ClusterIssuer

	pathType := networkingv1.PathTypePrefix
	ing.Spec.Rules = []networkingv1.IngressRule{{
		Host: domain.Spec.Host,
		IngressRuleValue: networkingv1.IngressRuleValue{
			HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path:     "/",
					PathType: &pathType,
					Backend: networkingv1.IngressBackend{
						Service: &networkingv1.IngressServiceBackend{
							Name: domain.Spec.AppRef.Name,
							// The named port survives spec.port changes on
							// the App without touching the Ingress.
							Port: networkingv1.ServiceBackendPort{Name: servicePortName},
						},
					},
				}},
			},
		},
	}}
	ing.Spec.TLS = []networkingv1.IngressTLS{{
		Hosts:      []string{domain.Spec.Host},
		SecretName: tlsSecretName(domain),
	}}
}

// tlsSecretName doubles as the Certificate name: ingress-shim names the
// Certificate it creates after spec.tls[].secretName.
func tlsSecretName(domain *orkanov1alpha1.Domain) string {
	return domain.Name + tlsSecretSuffix
}

// observeCertificate mirrors the ingress-shim-created Certificate's Ready
// condition; the operator never creates or mutates Certificates (ADR-0006).
func (r *DomainReconciler) observeCertificate(ctx context.Context, domain *orkanov1alpha1.Domain) (metav1.ConditionStatus, string, string, error) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	err := r.Get(ctx, types.NamespacedName{Namespace: domain.Namespace, Name: tlsSecretName(domain)}, cert)
	if apierrors.IsNotFound(err) {
		return metav1.ConditionUnknown, reasonCertificatePending,
			fmt.Sprintf("waiting for cert-manager to create Certificate %s", tlsSecretName(domain)), nil
	}
	if err != nil {
		return "", "", "", err
	}
	conditions, _, err := unstructured.NestedSlice(cert.Object, "status", "conditions")
	if err != nil {
		return "", "", "", fmt.Errorf("reading Certificate %s conditions: %w", cert.GetName(), err)
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		status, _ := cond["status"].(string)
		reason, _ := cond["reason"].(string)
		message, _ := cond["message"].(string)
		if reason == "" {
			reason = "Unknown"
		}
		switch metav1.ConditionStatus(status) {
		case metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown:
			return metav1.ConditionStatus(status), reason, message, nil
		}
	}
	return metav1.ConditionUnknown, reasonCertificatePending,
		fmt.Sprintf("Certificate %s has no Ready condition yet", tlsSecretName(domain)), nil
}

// updateAppURL derives App.status.url from the oldest live, conflict-winning
// Domain pointing at the App. The App reconciler never touches this field —
// single writer per field, mirroring the App/Domain split. The App is read
// uncached: the equality short-circuit must compare against server truth,
// because on the finalizer path a wrong skip is never retried — the Domain
// is gone and no App event will ever map back to it.
func (r *DomainReconciler) updateAppURL(ctx context.Context, namespace, appName string) error {
	var app orkanov1alpha1.App
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: appName}, &app)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	url := ""
	if workloadType(&app) == orkanov1alpha1.WorkloadWeb {
		var domains orkanov1alpha1.DomainList
		if err := r.List(ctx, &domains, client.InNamespace(namespace),
			client.MatchingFields{domainAppNameIndex: appName}); err != nil {
			return err
		}
		var oldest *orkanov1alpha1.Domain
		for i := range domains.Items {
			d := &domains.Items[i]
			if !d.DeletionTimestamp.IsZero() {
				continue
			}
			winner, err := r.hostWinner(ctx, d.Spec.Host)
			if err != nil {
				return err
			}
			if winner.Namespace != d.Namespace || winner.Name != d.Name {
				continue
			}
			if oldest == nil || olderDomain(d, oldest) {
				oldest = d
			}
		}
		if oldest != nil {
			url = "https://" + oldest.Spec.Host
		}
	}

	if app.Status.URL == url {
		return nil
	}
	app.Status.URL = url
	if err := r.Status().Update(ctx, &app); err != nil {
		return fmt.Errorf("updating App %s status.url: %w", appName, err)
	}
	return nil
}

func setDomainReady(domain *orkanov1alpha1.Domain, status metav1.ConditionStatus, reason, message string) {
	setDomainCondition(domain, orkanov1alpha1.ConditionReady, status, reason, message)
}

func setDomainCondition(domain *orkanov1alpha1.Domain, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&domain.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: domain.Generation,
	})
}

// failReady mirrors the App reconciler's pattern: a permanent refusal (a
// foreign same-name Ingress) must be visible in the Phase 1 UI, while
// conflicts heal on retry and would only flap the condition.
func (r *DomainReconciler) failReady(ctx context.Context, domain *orkanov1alpha1.Domain, before *orkanov1alpha1.DomainStatus, reason string, err error) error {
	if apierrors.IsConflict(err) {
		return err
	}
	setDomainReady(domain, metav1.ConditionFalse, reason, err.Error())
	if statusErr := r.updateStatus(ctx, domain, before); statusErr != nil {
		logf.FromContext(ctx).Error(statusErr, "failed to record failure condition", "reason", reason)
	}
	return err
}

func (r *DomainReconciler) updateStatus(ctx context.Context, domain *orkanov1alpha1.Domain, before *orkanov1alpha1.DomainStatus) error {
	domain.Status.ObservedGeneration = domain.Generation
	if equality.Semantic.DeepEqual(before, &domain.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, domain); err != nil {
		return fmt.Errorf("updating Domain status: %w", err)
	}
	return nil
}

func (r *DomainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if err := indexer.IndexField(context.Background(), &orkanov1alpha1.Domain{}, domainHostIndex,
		func(obj client.Object) []string {
			domain, ok := obj.(*orkanov1alpha1.Domain)
			if !ok {
				return nil
			}
			return []string{domain.Spec.Host}
		}); err != nil {
		return fmt.Errorf("indexing Domains by %s: %w", domainHostIndex, err)
	}
	if err := indexer.IndexField(context.Background(), &orkanov1alpha1.Domain{}, domainAppNameIndex,
		func(obj client.Object) []string {
			domain, ok := obj.(*orkanov1alpha1.Domain)
			if !ok {
				return nil
			}
			return []string{domain.Spec.AppRef.Name}
		}); err != nil {
		return fmt.Errorf("indexing Domains by %s: %w", domainAppNameIndex, err)
	}

	// Watched as unstructured (no cert-manager module); the CRD must exist
	// before the operator starts — orkano init installs cert-manager first.
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&orkanov1alpha1.Domain{}).
		Owns(&networkingv1.Ingress{}).
		Watches(cert, handler.EnqueueRequestsFromMapFunc(mapCertificateToDomain)).
		Watches(&orkanov1alpha1.Domain{}, handler.EnqueueRequestsFromMapFunc(r.mapDomainToHostPeers)).
		Watches(&orkanov1alpha1.App{}, handler.EnqueueRequestsFromMapFunc(r.mapAppToDomains)).
		Named("domain").
		Complete(r)
}

// mapCertificateToDomain inverts tlsSecretName: ingress-shim names the
// Certificate after the TLS secret, which this controller derives from the
// Domain name.
func mapCertificateToDomain(_ context.Context, obj client.Object) []reconcile.Request {
	name, found := strings.CutSuffix(obj.GetName(), tlsSecretSuffix)
	if !found || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      name,
	}}}
}

// mapDomainToHostPeers re-enqueues the other Domains claiming the same host,
// so deleting a conflict winner promotes the oldest loser without polling.
func (r *DomainReconciler) mapDomainToHostPeers(ctx context.Context, obj client.Object) []reconcile.Request {
	domain, ok := obj.(*orkanov1alpha1.Domain)
	if !ok {
		return nil
	}
	var domains orkanov1alpha1.DomainList
	if err := r.List(ctx, &domains, client.MatchingFields{domainHostIndex: domain.Spec.Host}); err != nil {
		logf.FromContext(ctx).Error(err, "listing host peers", "host", domain.Spec.Host)
		return nil
	}
	var reqs []reconcile.Request
	for i := range domains.Items {
		d := &domains.Items[i]
		if d.Namespace == domain.Namespace && d.Name == domain.Name {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: d.Namespace, Name: d.Name}})
	}
	return reqs
}

// mapAppToDomains catches the Domain-before-App ordering: a Domain stuck on
// AppNotFound reconciles the moment its App appears or changes type.
func (r *DomainReconciler) mapAppToDomains(ctx context.Context, obj client.Object) []reconcile.Request {
	var domains orkanov1alpha1.DomainList
	if err := r.List(ctx, &domains, client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{domainAppNameIndex: obj.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "listing Domains for App", "app", obj.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(domains.Items))
	for i := range domains.Items {
		d := &domains.Items[i]
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: d.Namespace, Name: d.Name}})
	}
	return reqs
}
