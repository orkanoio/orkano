package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

const (
	// appLabel is written into every Deployment's immutable spec.selector;
	// renaming it breaks every existing app without a migration.
	appLabel        = "app.orkano.io/app"
	managedByLabel  = "app.kubernetes.io/managed-by"
	managedByValue  = "orkano"
	defaultWebPort  = int32(8080)
	portEnvName     = "PORT"
	containerName   = "app"
	servicePortName = "http"

	buildAppNameIndex = "spec.appName"

	reasonWaitingForBuild = "WaitingForBuild"
	reasonInvalidImage    = "InvalidImage"
	reasonReconcileError  = "ReconcileError"
	reasonProgressing     = "Progressing"
	reasonAvailable       = "Available"
)

type AppReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *AppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var app orkanov1alpha1.App
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Children carry ownerReferences, so deletion needs no finalizer work.
	if !app.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	statusBefore := app.Status.DeepCopy()
	if err := r.observeBuilds(ctx, &app); err != nil {
		return ctrl.Result{}, fmt.Errorf("observing Builds: %w", err)
	}

	// The image arrives from the newest succeeded Build. Until then there
	// is nothing to run.
	if app.Status.Image == "" {
		setReady(&app, metav1.ConditionFalse, reasonWaitingForBuild, "no successful Build has produced an image yet")
		return ctrl.Result{}, r.updateStatus(ctx, &app, statusBefore)
	}
	// INV-06: only digest-pinned references are ever rendered into pods.
	if !strings.Contains(app.Status.Image, "@sha256:") {
		return ctrl.Result{}, r.failReady(ctx, &app, statusBefore, reasonInvalidImage,
			fmt.Errorf("refusing non-digest-pinned image %q", app.Status.Image))
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		// Never adopt an object Orkano didn't create: mutating a foreign
		// Deployment hits the immutable selector and loops forever.
		if !deploy.CreationTimestamp.IsZero() && metav1.GetControllerOf(deploy) == nil {
			return fmt.Errorf("existing Deployment %s/%s is not managed by Orkano; rename the App or delete the Deployment", deploy.Namespace, deploy.Name)
		}
		r.mutateDeployment(&app, deploy)
		return controllerutil.SetControllerReference(&app, deploy, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, r.failReady(ctx, &app, statusBefore, reasonReconcileError,
			fmt.Errorf("reconciling Deployment: %w", err))
	}
	if op != controllerutil.OperationResultNone {
		log.Info("reconciled Deployment", "operation", op)
	}

	// A type flip away from Web must take the Service down with it.
	if workloadType(&app) != orkanov1alpha1.WorkloadWeb {
		var svc corev1.Service
		err := r.Get(ctx, req.NamespacedName, &svc)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return ctrl.Result{}, r.failReady(ctx, &app, statusBefore, reasonReconcileError,
				fmt.Errorf("checking Service for non-Web app: %w", err))
		case metav1.IsControlledBy(&svc, &app):
			if err := r.Delete(ctx, &svc); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, r.failReady(ctx, &app, statusBefore, reasonReconcileError,
					fmt.Errorf("deleting Service for non-Web app: %w", err))
			}
			log.Info("deleted Service for non-Web app")
		}
	} else {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}}
		op, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			if !svc.CreationTimestamp.IsZero() && metav1.GetControllerOf(svc) == nil {
				return fmt.Errorf("existing Service %s/%s is not managed by Orkano; rename the App or delete the Service", svc.Namespace, svc.Name)
			}
			mutateService(&app, svc)
			return controllerutil.SetControllerReference(&app, svc, r.Scheme)
		})
		if err != nil {
			return ctrl.Result{}, r.failReady(ctx, &app, statusBefore, reasonReconcileError,
				fmt.Errorf("reconciling Service: %w", err))
		}
		if op != controllerutil.OperationResultNone {
			log.Info("reconciled Service", "operation", op)
		}
	}

	app.Status.AvailableReplicas = deploy.Status.AvailableReplicas
	desired := int32(1)
	if app.Spec.Replicas != nil {
		desired = *app.Spec.Replicas
	}
	replicas := fmt.Sprintf("%d/%d replicas available", app.Status.AvailableReplicas, desired)
	if app.Status.AvailableReplicas >= desired {
		setReady(&app, metav1.ConditionTrue, reasonAvailable, replicas)
	} else {
		setReady(&app, metav1.ConditionFalse, reasonProgressing, replicas)
	}
	return ctrl.Result{}, r.updateStatus(ctx, &app, statusBefore)
}

// observeBuilds derives latestBuild and image from the Builds pointing at
// this App. Both only ever advance: pruned Builds must not regress status
// or tear down a running workload.
func (r *AppReconciler) observeBuilds(ctx context.Context, app *orkanov1alpha1.App) error {
	var builds orkanov1alpha1.BuildList
	if err := r.List(ctx, &builds, client.InNamespace(app.Namespace),
		client.MatchingFields{buildAppNameIndex: app.Name}); err != nil {
		return err
	}
	if len(builds.Items) == 0 {
		return nil
	}
	sort.Slice(builds.Items, func(i, j int) bool { return newerBuild(&builds.Items[i], &builds.Items[j]) })
	app.Status.LatestBuild = builds.Items[0].Name
	for i := range builds.Items {
		b := &builds.Items[i]
		if b.Status.Phase == orkanov1alpha1.BuildSucceeded && b.Status.Image != "" {
			app.Status.Image = b.Status.Image
			break
		}
	}
	return nil
}

// newerBuild orders by creationTimestamp; names break the second-granularity
// timestamp ties deterministically.
func newerBuild(a, b *orkanov1alpha1.Build) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return b.CreationTimestamp.Before(&a.CreationTimestamp)
	}
	return a.Name > b.Name
}

func setReady(app *orkanov1alpha1.App, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               orkanov1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: app.Generation,
	})
}

// failReady records why reconciliation stopped before surfacing the error —
// without the condition a permanent refusal (a foreign same-name Deployment,
// a tag-only image) would be invisible in the Phase 1 UI. Conflicts are
// skipped: they heal on the immediate retry and would only flap the condition.
func (r *AppReconciler) failReady(ctx context.Context, app *orkanov1alpha1.App, before *orkanov1alpha1.AppStatus, reason string, err error) error {
	if apierrors.IsConflict(err) {
		return err
	}
	setReady(app, metav1.ConditionFalse, reason, err.Error())
	if statusErr := r.updateStatus(ctx, app, before); statusErr != nil {
		logf.FromContext(ctx).Error(statusErr, "failed to record failure condition", "reason", reason)
	}
	return err
}

// updateStatus writes status only when it changed: reconciles triggered by
// our own status writes must settle, not loop.
func (r *AppReconciler) updateStatus(ctx context.Context, app *orkanov1alpha1.App, before *orkanov1alpha1.AppStatus) error {
	app.Status.ObservedGeneration = app.Generation
	if equality.Semantic.DeepEqual(before, &app.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, app); err != nil {
		return fmt.Errorf("updating App status: %w", err)
	}
	return nil
}

func workloadType(app *orkanov1alpha1.App) orkanov1alpha1.WorkloadType {
	if app.Spec.Type == "" {
		return orkanov1alpha1.WorkloadWeb
	}
	return app.Spec.Type
}

func webPort(app *orkanov1alpha1.App) int32 {
	if app.Spec.Port != nil {
		return *app.Spec.Port
	}
	return defaultWebPort
}

func selectorLabels(app *orkanov1alpha1.App) map[string]string {
	return map[string]string{appLabel: app.Name}
}

// mutateDeployment assigns only the fields Orkano owns, leaving everything
// the apiserver defaulted (imagePullPolicy, terminationMessage*, pod-level
// policies) untouched so re-reconciles compare equal and skip the update.
func (r *AppReconciler) mutateDeployment(app *orkanov1alpha1.App, deploy *appsv1.Deployment) {
	labels := selectorLabels(app)

	if deploy.Labels == nil {
		deploy.Labels = map[string]string{}
	}
	deploy.Labels[appLabel] = app.Name
	deploy.Labels[managedByLabel] = managedByValue

	deploy.Spec.Replicas = app.Spec.Replicas
	deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	deploy.Spec.Template.Labels = labels

	if len(deploy.Spec.Template.Spec.Containers) != 1 || deploy.Spec.Template.Spec.Containers[0].Name != containerName {
		deploy.Spec.Template.Spec.Containers = []corev1.Container{{Name: containerName}}
	}
	container := &deploy.Spec.Template.Spec.Containers[0]
	container.Image = app.Status.Image
	container.Command = app.Spec.Command
	container.Env = containerEnv(app)
	container.Resources = containerResources(app.Spec.Resources)
	if workloadType(app) == orkanov1alpha1.WorkloadWeb {
		port := webPort(app)
		container.Ports = []corev1.ContainerPort{{
			Name:          servicePortName,
			ContainerPort: port,
			Protocol:      corev1.ProtocolTCP,
		}}
		container.ReadinessProbe, container.LivenessProbe = probes(app, port)
	} else {
		container.Ports = nil
		container.ReadinessProbe, container.LivenessProbe = nil, nil
	}
}

// containerEnv injects PORT to match the serving port unless the user set
// PORT explicitly — an explicit value is user intent, not drift.
func containerEnv(app *orkanov1alpha1.App) []corev1.EnvVar {
	env := make([]corev1.EnvVar, 0, len(app.Spec.Env)+1)
	userSetsPort := false
	for _, e := range app.Spec.Env {
		if e.Name == portEnvName {
			userSetsPort = true
		}
		v := corev1.EnvVar{Name: e.Name, Value: e.Value}
		if e.SecretRef != nil {
			v.Value = ""
			v.ValueFrom = &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.SecretRef.Name},
					Key:                  e.SecretRef.Key,
				},
			}
		}
		env = append(env, v)
	}
	if workloadType(app) == orkanov1alpha1.WorkloadWeb && !userSetsPort {
		env = append(env, corev1.EnvVar{Name: portEnvName, Value: fmt.Sprint(webPort(app))})
	}
	return env
}

// containerResources maps spec values to requests and derives limits
// (memory limit = request, no CPU limit) so defaults can improve without
// a stored-object migration.
func containerResources(res *orkanov1alpha1.Resources) corev1.ResourceRequirements {
	if res == nil {
		return corev1.ResourceRequirements{}
	}
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	if res.CPU != nil {
		requests[corev1.ResourceCPU] = *res.CPU
	}
	if res.Memory != nil {
		requests[corev1.ResourceMemory] = *res.Memory
		limits[corev1.ResourceMemory] = *res.Memory
	}
	out := corev1.ResourceRequirements{}
	if len(requests) > 0 {
		out.Requests = requests
	}
	if len(limits) > 0 {
		out.Limits = limits
	}
	return out
}

// probes carry their server-side defaults (successThreshold, HTTP scheme)
// explicitly so a freshly built probe compares equal to the stored one.
func probes(app *orkanov1alpha1.App, port int32) (readiness, liveness *corev1.Probe) {
	if hc := app.Spec.HealthCheck; hc != nil {
		httpGet := corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   hc.Path,
				Port:   intstr.FromInt32(port),
				Scheme: corev1.URISchemeHTTP,
			},
		}
		readiness = &corev1.Probe{
			ProbeHandler:     httpGet,
			PeriodSeconds:    10,
			TimeoutSeconds:   2,
			SuccessThreshold: 1,
			FailureThreshold: 3,
		}
		liveness = &corev1.Probe{
			ProbeHandler:     httpGet,
			PeriodSeconds:    20,
			TimeoutSeconds:   2,
			SuccessThreshold: 1,
			FailureThreshold: 3,
		}
		return readiness, liveness
	}
	readiness = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
		PeriodSeconds:    10,
		TimeoutSeconds:   2,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
	return readiness, nil
}

func mutateService(app *orkanov1alpha1.App, svc *corev1.Service) {
	if svc.Labels == nil {
		svc.Labels = map[string]string{}
	}
	svc.Labels[appLabel] = app.Name
	svc.Labels[managedByLabel] = managedByValue

	// ClusterIP is pinned so exposure drift (a tampered NodePort or
	// LoadBalancer) is healed; all external traffic goes through Ingress.
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	svc.Spec.Selector = selectorLabels(app)
	// The named target keeps routing correct mid-rollout when spec.port
	// changes: old and new pods each resolve "http" to their own port.
	svc.Spec.Ports = []corev1.ServicePort{{
		Name:       servicePortName,
		Port:       80,
		TargetPort: intstr.FromString(servicePortName),
		Protocol:   corev1.ProtocolTCP,
	}}
}

func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &orkanov1alpha1.Build{}, buildAppNameIndex,
		func(obj client.Object) []string {
			build, ok := obj.(*orkanov1alpha1.Build)
			if !ok {
				return nil
			}
			return []string{build.Spec.AppName}
		}); err != nil {
		return fmt.Errorf("indexing Builds by %s: %w", buildAppNameIndex, err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&orkanov1alpha1.App{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Watches(&orkanov1alpha1.Build{}, handler.EnqueueRequestsFromMapFunc(mapBuildToApp)).
		Named("app").
		Complete(r)
}

// mapBuildToApp points Build events at the App they belong to, so a
// finished build rolls the workload without polling.
func mapBuildToApp(_ context.Context, obj client.Object) []reconcile.Request {
	build, ok := obj.(*orkanov1alpha1.Build)
	if !ok || build.Spec.AppName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: build.Namespace,
		Name:      build.Spec.AppName,
	}}}
}
