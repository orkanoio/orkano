package controller

import (
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	registryNamespace       = systemNamespace // canonical const lives in cache.go
	registryCertificateName = "orkano-registry-tls"
	registryDeploymentName  = "orkano-registry"

	// registryCertRevisionAnnotation on the registry pod template records the
	// Certificate revision the running pod was started for; converging it to
	// status.revision is what triggers the rollout.
	registryCertRevisionAnnotation = "orkano.io/registry-cert-revision"
)

// RegistryCertReconciler rolls the in-cluster registry Deployment whenever
// cert-manager reissues its TLS certificate: distribution loads the keypair
// exactly once at startup (tls.LoadX509KeyPair, no hot reload), so a renewed
// Secret never reaches a running pod. The signal is the Certificate's
// status.revision, which cert-manager bumps on every issuance — deliberately
// not the Secret itself, so this rotation path reads no Secret. The operator's
// only Secret read in orkano-system is the GitHub App key (githubapp, INV-07),
// unrelated to cert rotation.
type RegistryCertReconciler struct {
	client.Client
}

func (r *RegistryCertReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	if err := r.Get(ctx, req.NamespacedName, cert); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	revision, found, err := unstructured.NestedInt64(cert.Object, "status", "revision")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reading Certificate %s status.revision: %w", cert.GetName(), err)
	}
	if !found {
		// Nothing issued yet; the next status update re-triggers.
		return ctrl.Result{}, nil
	}

	var deploy appsv1.Deployment
	err = r.Get(ctx, types.NamespacedName{Namespace: registryNamespace, Name: registryDeploymentName}, &deploy)
	if apierrors.IsNotFound(err) {
		// Registry not installed (yet); the Deployment watch covers its arrival.
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("fetching registry Deployment: %w", err)
	}

	want := strconv.FormatInt(revision, 10)
	if deploy.Spec.Template.Annotations[registryCertRevisionAnnotation] == want {
		return ctrl.Result{}, nil
	}
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = map[string]string{}
	}
	// First adoption of a pre-existing Deployment causes one redundant roll
	// (the pod may already hold the current cert, but only the Secret could
	// prove it). Accepted: it converges once and the registry is idle then.
	deploy.Spec.Template.Annotations[registryCertRevisionAnnotation] = want
	if err := r.Update(ctx, &deploy); err != nil {
		return ctrl.Result{}, fmt.Errorf("rolling registry Deployment to cert revision %s: %w", want, err)
	}
	logf.FromContext(ctx).Info("rolled registry Deployment for reissued TLS certificate", "revision", want)
	return ctrl.Result{}, nil
}

func (r *RegistryCertReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	isRegistryCert := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == registryNamespace && obj.GetName() == registryCertificateName
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(cert, builder.WithPredicates(isRegistryCert)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(mapRegistryDeployment)).
		Named("registrycert").
		Complete(r)
}

// mapRegistryDeployment heals annotation drift and covers the Deployment
// being created after the Certificate already has a revision.
func mapRegistryDeployment(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != registryNamespace || obj.GetName() != registryDeploymentName {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: registryNamespace,
		Name:      registryCertificateName,
	}}}
}
