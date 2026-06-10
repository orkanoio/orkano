package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	spikev1 "orkano-spike-controller/api/v1"
)

const FinalizerName = "spike.orkano.io/finalizer"

type AppSpikeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *AppSpikeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var app spikev1.AppSpike
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !app.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&app, FinalizerName) {
			log.Info("cleanup before deletion", "message", app.Spec.Message)
			controllerutil.RemoveFinalizer(&app, FinalizerName)
			if err := r.Update(ctx, &app); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&app, FinalizerName) {
		controllerutil.AddFinalizer(&app, FinalizerName)
		if err := r.Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
		// the update event triggers the next reconcile, which sets status
		return ctrl.Result{}, nil
	}

	changed := meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "MessageAccepted",
		Message:            app.Spec.Message,
		ObservedGeneration: app.Generation,
	})
	if changed || app.Status.ObservedGeneration != app.Generation {
		app.Status.ObservedGeneration = app.Generation
		if err := r.Status().Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *AppSpikeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spikev1.AppSpike{}).
		Named("appspike").
		Complete(r)
}
