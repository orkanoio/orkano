package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

const testImage = "registry.orkano-system.svc.cluster.local/apps/api@sha256:6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eebdaab7c610cf7d66709b3b"

func quantity(t *testing.T, s string) *resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	if err != nil {
		t.Fatalf("bad quantity %q: %v", s, err)
	}
	return &q
}

func createWebApp(t *testing.T, name string, mutate func(*orkanov1alpha1.App)) *orkanov1alpha1.App {
	t.Helper()
	ctx := context.Background()
	app := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec: orkanov1alpha1.AppSpec{
			Source: orkanov1alpha1.Source{
				GitHub: orkanov1alpha1.GitHubSource{Repo: "orkanoio/example"},
			},
			Build: orkanov1alpha1.BuildStrategy{Strategy: "Dockerfile"},
		},
	}
	if mutate != nil {
		mutate(app)
	}
	if err := k8sClient.Create(ctx, app); err != nil {
		t.Fatalf("failed to create App %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, app) })
	return app
}

func setImage(t *testing.T, app *orkanov1alpha1.App, image string) {
	t.Helper()
	ctx := context.Background()
	key := types.NamespacedName{Name: app.Name, Namespace: app.Namespace}
	var fresh orkanov1alpha1.App
	if err := k8sClient.Get(ctx, key, &fresh); err != nil {
		t.Fatalf("failed to refetch App: %v", err)
	}
	fresh.Status.Image = image
	if err := k8sClient.Status().Update(ctx, &fresh); err != nil {
		t.Fatalf("failed to set status.image: %v", err)
	}
}

func getDeployment(t *testing.T, name string) *appsv1.Deployment {
	t.Helper()
	var deploy appsv1.Deployment
	key := types.NamespacedName{Name: name, Namespace: appsNamespace}
	eventually(t, "Deployment "+name, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	return &deploy
}

func TestWebAppRendersDeploymentAndService(t *testing.T) {
	replicas := int32(2)
	port := int32(3000)
	app := createWebApp(t, "web-full", func(a *orkanov1alpha1.App) {
		a.Spec.Port = &port
		a.Spec.Replicas = &replicas
		a.Spec.Command = []string{"/bin/server"}
		a.Spec.Env = []orkanov1alpha1.EnvVar{
			{Name: "DATABASE_URL", SecretRef: &orkanov1alpha1.SecretKeyRef{Name: "api-db", Key: "url"}},
			{Name: "NODE_ENV", Value: "production"},
		}
		a.Spec.HealthCheck = &orkanov1alpha1.HealthCheck{Path: "/healthz"}
		a.Spec.Resources = &orkanov1alpha1.Resources{
			CPU:    quantity(t, "250m"),
			Memory: quantity(t, "512Mi"),
		}
	})
	setImage(t, app, testImage)

	deploy := getDeployment(t, app.Name)

	owner := metav1.GetControllerOf(deploy)
	if owner == nil || owner.Kind != "App" || owner.Name != app.Name {
		t.Fatalf("Deployment not controller-owned by the App: %+v", owner)
	}
	if *deploy.Spec.Replicas != replicas {
		t.Errorf("replicas = %d, want %d", *deploy.Spec.Replicas, replicas)
	}

	c := deploy.Spec.Template.Spec.Containers[0]
	if c.Image != testImage {
		t.Errorf("image = %q, want %q", c.Image, testImage)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != port {
		t.Errorf("ports = %+v, want one port %d", c.Ports, port)
	}
	if len(c.Command) != 1 || c.Command[0] != "/bin/server" {
		t.Errorf("command = %v, want [/bin/server]", c.Command)
	}

	envByName := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		envByName[e.Name] = e
	}
	if got := envByName["PORT"].Value; got != "3000" {
		t.Errorf("PORT env = %q, want 3000", got)
	}
	if got := envByName["NODE_ENV"].Value; got != "production" {
		t.Errorf("NODE_ENV env = %q, want production", got)
	}
	dbURL := envByName["DATABASE_URL"]
	if dbURL.ValueFrom == nil || dbURL.ValueFrom.SecretKeyRef == nil ||
		dbURL.ValueFrom.SecretKeyRef.Name != "api-db" || dbURL.ValueFrom.SecretKeyRef.Key != "url" {
		t.Errorf("DATABASE_URL not mapped to secretKeyRef api-db/url: %+v", dbURL)
	}

	if got := c.Resources.Requests.Cpu().String(); got != "250m" {
		t.Errorf("cpu request = %s, want 250m", got)
	}
	if got := c.Resources.Requests.Memory().String(); got != "512Mi" {
		t.Errorf("memory request = %s, want 512Mi", got)
	}
	if got := c.Resources.Limits.Memory().String(); got != "512Mi" {
		t.Errorf("memory limit = %s, want 512Mi (= request)", got)
	}
	if _, hasCPULimit := c.Resources.Limits[corev1.ResourceCPU]; hasCPULimit {
		t.Errorf("cpu limit set, want none: %+v", c.Resources.Limits)
	}

	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/healthz" ||
		c.ReadinessProbe.HTTPGet.Port.IntValue() != int(port) {
		t.Errorf("readiness probe = %+v, want HTTP GET /healthz on %d", c.ReadinessProbe, port)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil || c.LivenessProbe.HTTPGet.Path != "/healthz" ||
		c.LivenessProbe.HTTPGet.Port.IntValue() != int(port) {
		t.Errorf("liveness probe = %+v, want HTTP GET /healthz on %d", c.LivenessProbe, port)
	}

	var svc corev1.Service
	key := types.NamespacedName{Name: app.Name, Namespace: appsNamespace}
	eventually(t, "Service", func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, key, &svc)
		return err == nil, client.IgnoreNotFound(err)
	})
	if svcOwner := metav1.GetControllerOf(&svc); svcOwner == nil || svcOwner.Name != app.Name {
		t.Fatalf("Service not controller-owned by the App: %+v", svcOwner)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.String() != servicePortName {
		t.Errorf("service ports = %+v, want 80 -> named port %q", svc.Spec.Ports, servicePortName)
	}
	if svc.Spec.Selector[appLabel] != app.Name {
		t.Errorf("service selector = %+v, want %s=%s", svc.Spec.Selector, appLabel, app.Name)
	}
	if !equality.Semantic.DeepEqual(svc.Spec.Selector, deploy.Spec.Template.Labels) {
		t.Errorf("service selector %+v does not match pod template labels %+v", svc.Spec.Selector, deploy.Spec.Template.Labels)
	}
}

func TestWebAppDefaultsPortAndTCPProbe(t *testing.T) {
	app := createWebApp(t, "web-defaults", nil)
	setImage(t, app, testImage)

	deploy := getDeployment(t, app.Name)
	c := deploy.Spec.Template.Spec.Containers[0]
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != defaultWebPort {
		t.Errorf("ports = %+v, want default %d", c.Ports, defaultWebPort)
	}
	var portEnv string
	for _, e := range c.Env {
		if e.Name == "PORT" {
			portEnv = e.Value
		}
	}
	if portEnv != "8080" {
		t.Errorf("PORT env = %q, want 8080", portEnv)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil ||
		c.ReadinessProbe.TCPSocket.Port.IntValue() != int(defaultWebPort) {
		t.Errorf("readiness probe = %+v, want TCPSocket on %d", c.ReadinessProbe, defaultWebPort)
	}
	if c.LivenessProbe != nil {
		t.Errorf("liveness probe = %+v, want none without healthCheck", c.LivenessProbe)
	}
}

func TestAppWithoutImageRendersNothing(t *testing.T) {
	app := createWebApp(t, "web-no-image", nil)

	time.Sleep(1500 * time.Millisecond)
	var deploy appsv1.Deployment
	err := k8sClient.Get(context.Background(), types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &deploy)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no Deployment before a build supplies an image, got: %v", err)
	}

	setImage(t, app, "ghcr.io/orkanoio/example:latest")
	time.Sleep(1500 * time.Millisecond)
	err = k8sClient.Get(context.Background(), types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &deploy)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected tag-only image to be refused (INV-06), got: %v", err)
	}
}

func TestWebAppHealsDriftAndStaysStable(t *testing.T) {
	ctx := context.Background()
	app := createWebApp(t, "web-drift", nil)
	setImage(t, app, testImage)
	deploy := getDeployment(t, app.Name)

	tampered := deploy.DeepCopy()
	five := int32(5)
	tampered.Spec.Replicas = &five
	if err := k8sClient.Update(ctx, tampered); err != nil {
		t.Fatalf("failed to tamper with Deployment: %v", err)
	}
	eventually(t, "drift to be healed", func(ctx context.Context) (bool, error) {
		var got appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &got); err != nil {
			return false, err
		}
		return *got.Spec.Replicas == 1, nil
	})

	// The full mutate closures must be no-ops against the server-defaulted
	// stored objects — CreateOrUpdate compares whole objects, metadata
	// included, and any diff means a spurious update every reconcile.
	r := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	var fresh orkanov1alpha1.App
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &fresh); err != nil {
		t.Fatalf("failed to refetch App: %v", err)
	}
	var stored appsv1.Deployment
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &stored); err != nil {
		t.Fatalf("failed to refetch Deployment: %v", err)
	}
	before := stored.DeepCopy()
	r.mutateDeployment(&fresh, &stored)
	if err := controllerutil.SetControllerReference(&fresh, &stored, r.Scheme); err != nil {
		t.Fatalf("failed to set controller reference: %v", err)
	}
	if !equality.Semantic.DeepEqual(before, &stored) {
		t.Errorf("Deployment mutate closure is not stable against the stored object:\nbefore: %+v\nafter:  %+v", before, &stored)
	}

	var storedSvc corev1.Service
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &storedSvc); err != nil {
		t.Fatalf("failed to refetch Service: %v", err)
	}
	beforeSvc := storedSvc.DeepCopy()
	mutateService(&fresh, &storedSvc)
	if err := controllerutil.SetControllerReference(&fresh, &storedSvc, r.Scheme); err != nil {
		t.Fatalf("failed to set controller reference: %v", err)
	}
	if !equality.Semantic.DeepEqual(beforeSvc, &storedSvc) {
		t.Errorf("Service mutate closure is not stable against the stored object:\nbefore: %+v\nafter:  %+v", beforeSvc, &storedSvc)
	}
}

func TestWebAppUserPortEnvWinsOverInjection(t *testing.T) {
	app := createWebApp(t, "web-user-port", func(a *orkanov1alpha1.App) {
		a.Spec.Env = []orkanov1alpha1.EnvVar{{Name: "PORT", Value: "9000"}}
	})
	setImage(t, app, testImage)

	deploy := getDeployment(t, app.Name)
	c := deploy.Spec.Template.Spec.Containers[0]
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != defaultWebPort {
		t.Errorf("ports = %+v, want default %d", c.Ports, defaultWebPort)
	}
	var portValues []string
	for _, e := range c.Env {
		if e.Name == "PORT" {
			portValues = append(portValues, e.Value)
		}
	}
	if len(portValues) != 1 || portValues[0] != "9000" {
		t.Errorf("PORT env entries = %v, want exactly one entry with value 9000", portValues)
	}
}

func TestTypeFlipToWorkerDeletesService(t *testing.T) {
	ctx := context.Background()
	app := createWebApp(t, "web-to-worker", nil)
	setImage(t, app, testImage)
	getDeployment(t, app.Name)

	key := types.NamespacedName{Name: app.Name, Namespace: appsNamespace}
	var svc corev1.Service
	eventually(t, "Service to exist", func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, key, &svc)
		return err == nil, client.IgnoreNotFound(err)
	})

	var fresh orkanov1alpha1.App
	if err := k8sClient.Get(ctx, key, &fresh); err != nil {
		t.Fatalf("failed to refetch App: %v", err)
	}
	fresh.Spec.Type = orkanov1alpha1.WorkloadWorker
	fresh.Spec.Env = nil
	if err := k8sClient.Update(ctx, &fresh); err != nil {
		t.Fatalf("failed to flip App to Worker: %v", err)
	}

	eventually(t, "Service to be deleted", func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, key, &svc)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}
