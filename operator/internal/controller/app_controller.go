package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

const (
	appLabel        = "app.orkano.io/app"
	managedByLabel  = "app.kubernetes.io/managed-by"
	managedByValue  = "orkano"
	defaultWebPort  = int32(8080)
	portEnvName     = "PORT"
	containerName   = "app"
	servicePortName = "http"
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

	// The image arrives via status from the newest succeeded Build. Until
	// then there is nothing to run; the status update triggers reconcile.
	if app.Status.Image == "" {
		log.Info("no image yet, skipping workload render")
		return ctrl.Result{}, nil
	}
	// INV-06: only digest-pinned references are ever rendered into pods.
	if !strings.Contains(app.Status.Image, "@sha256:") {
		return ctrl.Result{}, fmt.Errorf("refusing non-digest-pinned image %q", app.Status.Image)
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		r.mutateDeployment(&app, deploy)
		return controllerutil.SetControllerReference(&app, deploy, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Deployment: %w", err)
	}
	if op != controllerutil.OperationResultNone {
		log.Info("reconciled Deployment", "operation", op)
	}

	if workloadType(&app) != orkanov1alpha1.WorkloadWeb {
		return ctrl.Result{}, nil
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}}
	op, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		mutateService(&app, svc)
		return controllerutil.SetControllerReference(&app, svc, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling Service: %w", err)
	}
	if op != controllerutil.OperationResultNone {
		log.Info("reconciled Service", "operation", op)
	}

	return ctrl.Result{}, nil
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

	svc.Spec.Selector = selectorLabels(app)
	svc.Spec.Ports = []corev1.ServicePort{{
		Name:       servicePortName,
		Port:       80,
		TargetPort: intstr.FromInt32(webPort(app)),
		Protocol:   corev1.ProtocolTCP,
	}}
}

func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orkanov1alpha1.App{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("app").
		Complete(r)
}
