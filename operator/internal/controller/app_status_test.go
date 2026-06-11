package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

const testImage2 = "registry.orkano-system.svc.cluster.local/apps/api@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func createBuild(t *testing.T, name, appName string) *orkanov1alpha1.Build {
	t.Helper()
	ctx := context.Background()
	build := &orkanov1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
		Spec: orkanov1alpha1.BuildSpec{
			AppName: appName,
			Commit:  "0123456789abcdef0123456789abcdef01234567",
			Source: orkanov1alpha1.Source{
				GitHub: orkanov1alpha1.GitHubSource{Repo: "orkanoio/example"},
			},
			Strategy: orkanov1alpha1.BuildStrategy{Strategy: "Dockerfile"},
		},
	}
	if err := k8sClient.Create(ctx, build); err != nil {
		t.Fatalf("failed to create Build %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, build) })
	return build
}

func setBuildPhase(t *testing.T, name string, phase orkanov1alpha1.BuildPhase, image string) {
	t.Helper()
	ctx := context.Background()
	var build orkanov1alpha1.Build
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appsNamespace}, &build); err != nil {
		t.Fatalf("failed to refetch Build %s: %v", name, err)
	}
	build.Status.Phase = phase
	build.Status.Image = image
	if err := k8sClient.Status().Update(ctx, &build); err != nil {
		t.Fatalf("failed to set Build %s status: %v", name, err)
	}
}

// markDeploymentAvailable plays the part of the deployment controller, which
// envtest does not run.
func markDeploymentAvailable(t *testing.T, name string, available int32) {
	t.Helper()
	eventually(t, "Deployment status update for "+name, func(ctx context.Context) (bool, error) {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appsNamespace}, &deploy); err != nil {
			return false, err
		}
		deploy.Status.ObservedGeneration = deploy.Generation
		deploy.Status.Replicas = available
		deploy.Status.UpdatedReplicas = available
		deploy.Status.ReadyReplicas = available
		deploy.Status.AvailableReplicas = available
		if err := k8sClient.Status().Update(ctx, &deploy); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil // raced a concurrent reconcile; retry
			}
			return false, err
		}
		return true, nil
	})
}

func waitForReady(t *testing.T, appName string, status metav1.ConditionStatus, reason string) *orkanov1alpha1.App {
	t.Helper()
	var app orkanov1alpha1.App
	eventually(t, fmt.Sprintf("App %s Ready=%s/%s", appName, status, reason), func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: appName, Namespace: appsNamespace}, &app); err != nil {
			return false, err
		}
		cond := meta.FindStatusCondition(app.Status.Conditions, orkanov1alpha1.ConditionReady)
		return cond != nil && cond.Status == status && cond.Reason == reason &&
			cond.ObservedGeneration == app.Generation &&
			app.Status.ObservedGeneration == app.Generation, nil
	})
	return &app
}

func TestAppStatusLifecycle(t *testing.T) {
	ctx := context.Background()
	app := createApp(t, "status-lifecycle", nil)

	waitForReady(t, app.Name, metav1.ConditionFalse, reasonWaitingForBuild)

	createBuild(t, "status-lifecycle-001", app.Name)
	setBuildPhase(t, "status-lifecycle-001", orkanov1alpha1.BuildSucceeded, testImage)

	eventually(t, "image and latestBuild from the succeeded Build", func(ctx context.Context) (bool, error) {
		var got orkanov1alpha1.App
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &got); err != nil {
			return false, err
		}
		return got.Status.Image == testImage && got.Status.LatestBuild == "status-lifecycle-001", nil
	})
	getDeployment(t, app.Name)

	got := waitForReady(t, app.Name, metav1.ConditionFalse, reasonProgressing)
	if got.Status.AvailableReplicas != 0 {
		t.Errorf("availableReplicas = %d, want 0 before the Deployment reports", got.Status.AvailableReplicas)
	}

	markDeploymentAvailable(t, app.Name, 1)
	got = waitForReady(t, app.Name, metav1.ConditionTrue, reasonAvailable)
	if got.Status.AvailableReplicas != 1 {
		t.Errorf("availableReplicas = %d, want 1 mirrored from the Deployment", got.Status.AvailableReplicas)
	}

	// A newer Build that has not succeeded moves latestBuild but never the
	// rolled-out image.
	createBuild(t, "status-lifecycle-002", app.Name)
	eventually(t, "latestBuild to advance to the pending Build", func(ctx context.Context) (bool, error) {
		var fresh orkanov1alpha1.App
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &fresh); err != nil {
			return false, err
		}
		return fresh.Status.LatestBuild == "status-lifecycle-002", nil
	})
	setBuildPhase(t, "status-lifecycle-002", orkanov1alpha1.BuildFailed, "")
	time.Sleep(1500 * time.Millisecond)
	var fresh orkanov1alpha1.App
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &fresh); err != nil {
		t.Fatalf("failed to refetch App: %v", err)
	}
	if fresh.Status.Image != testImage {
		t.Errorf("image = %q after a failed Build, want unchanged %q", fresh.Status.Image, testImage)
	}

	createBuild(t, "status-lifecycle-003", app.Name)
	setBuildPhase(t, "status-lifecycle-003", orkanov1alpha1.BuildSucceeded, testImage2)
	eventually(t, "image to advance with the new succeeded Build", func(ctx context.Context) (bool, error) {
		var fresh orkanov1alpha1.App
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &fresh); err != nil {
			return false, err
		}
		return fresh.Status.Image == testImage2 && fresh.Status.LatestBuild == "status-lifecycle-003", nil
	})
	eventually(t, "Deployment to roll to the new digest", func(ctx context.Context) (bool, error) {
		var deploy appsv1.Deployment
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &deploy); err != nil {
			return false, err
		}
		return deploy.Spec.Template.Spec.Containers[0].Image == testImage2, nil
	})

	// Status writes triggered by our own status writes must settle.
	waitForReady(t, app.Name, metav1.ConditionTrue, reasonAvailable)
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &fresh); err != nil {
		t.Fatalf("failed to refetch App: %v", err)
	}
	rv := fresh.ResourceVersion
	time.Sleep(1500 * time.Millisecond)
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &fresh); err != nil {
		t.Fatalf("failed to refetch App: %v", err)
	}
	if fresh.ResourceVersion != rv {
		t.Errorf("App resourceVersion churned from %s to %s with no input change", rv, fresh.ResourceVersion)
	}
}

func TestAppStatusSurvivesBuildPruning(t *testing.T) {
	ctx := context.Background()
	app := createApp(t, "status-pruned", nil)
	build := createBuild(t, "status-pruned-001", app.Name)
	setBuildPhase(t, build.Name, orkanov1alpha1.BuildSucceeded, testImage)

	eventually(t, "status from the succeeded Build", func(ctx context.Context) (bool, error) {
		var got orkanov1alpha1.App
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &got); err != nil {
			return false, err
		}
		return got.Status.Image == testImage && got.Status.LatestBuild == build.Name, nil
	})

	if err := k8sClient.Delete(ctx, build); err != nil {
		t.Fatalf("failed to delete Build: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	var got orkanov1alpha1.App
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: appsNamespace}, &got); err != nil {
		t.Fatalf("failed to refetch App: %v", err)
	}
	if got.Status.Image != testImage || got.Status.LatestBuild != build.Name {
		t.Errorf("status regressed after Build pruning: image=%q latestBuild=%q, want %q/%q",
			got.Status.Image, got.Status.LatestBuild, testImage, build.Name)
	}
}

func TestForeignDeploymentSetsReconcileErrorCondition(t *testing.T) {
	ctx := context.Background()
	foreign := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "status-foreign", Namespace: appsNamespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "not-orkano"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "not-orkano"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "example.com/foreign:1"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, foreign); err != nil {
		t.Fatalf("failed to create foreign Deployment: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

	app := createApp(t, "status-foreign", nil)
	setImage(t, app, testImage)

	got := waitForReady(t, app.Name, metav1.ConditionFalse, reasonReconcileError)
	cond := meta.FindStatusCondition(got.Status.Conditions, orkanov1alpha1.ConditionReady)
	if !strings.Contains(cond.Message, "not managed by Orkano") {
		t.Errorf("condition message = %q, want the foreign-Deployment refusal to be visible in the UI", cond.Message)
	}
}

func TestWorkerAppReadyLifecycle(t *testing.T) {
	app := createApp(t, "status-worker", func(a *orkanov1alpha1.App) {
		a.Spec.Type = orkanov1alpha1.WorkloadWorker
	})

	waitForReady(t, app.Name, metav1.ConditionFalse, reasonWaitingForBuild)

	createBuild(t, "status-worker-001", app.Name)
	setBuildPhase(t, "status-worker-001", orkanov1alpha1.BuildSucceeded, testImage)
	getDeployment(t, app.Name)
	got := waitForReady(t, app.Name, metav1.ConditionFalse, reasonProgressing)
	if got.Status.LatestBuild != "status-worker-001" || got.Status.Image != testImage {
		t.Errorf("latestBuild/image = %q/%q, want status-worker-001/%q", got.Status.LatestBuild, got.Status.Image, testImage)
	}

	markDeploymentAvailable(t, app.Name, 1)
	got = waitForReady(t, app.Name, metav1.ConditionTrue, reasonAvailable)
	if got.Status.AvailableReplicas != 1 {
		t.Errorf("availableReplicas = %d, want 1 mirrored from the Deployment", got.Status.AvailableReplicas)
	}
}

// TestAppPrinterColumnsRender asks the apiserver for the Table that kubectl
// get apps would print — the acceptance bar for the Phase 1 UI, probed, not
// inferred from the CRD yaml.
func TestAppPrinterColumnsRender(t *testing.T) {
	ctx := context.Background()
	app := createApp(t, "status-columns", nil)
	createBuild(t, "status-columns-001", app.Name)
	setBuildPhase(t, "status-columns-001", orkanov1alpha1.BuildSucceeded, testImage)
	getDeployment(t, app.Name)
	markDeploymentAvailable(t, app.Name, 1)
	waitForReady(t, app.Name, metav1.ConditionTrue, reasonAvailable)

	cfg := rest.CopyConfig(restConfig)
	cfg.GroupVersion = &schema.GroupVersion{
		Group:   orkanov1alpha1.GroupVersion.Group,
		Version: orkanov1alpha1.GroupVersion.Version,
	}
	cfg.APIPath = "/apis"
	cfg.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatalf("failed to build REST client: %v", err)
	}
	raw, err := rc.Get().Resource("apps").Namespace(appsNamespace).Name(app.Name).
		SetHeader("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io").
		DoRaw(ctx)
	if err != nil {
		t.Fatalf("Table request failed: %v", err)
	}
	var table metav1.Table
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("failed to decode Table response: %v", err)
	}

	idx := map[string]int{}
	for i, col := range table.ColumnDefinitions {
		idx[col.Name] = i
	}
	for _, name := range []string{"Name", "Type", "Ready", "Replicas", "URL", "Age"} {
		if _, ok := idx[name]; !ok {
			t.Fatalf("printer column %q missing from Table response: %+v", name, table.ColumnDefinitions)
		}
	}
	if len(table.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(table.Rows))
	}
	cells := table.Rows[0].Cells

	if got := fmt.Sprint(cells[idx["Name"]]); got != app.Name {
		t.Errorf("Name column = %q, want %q", got, app.Name)
	}
	if got := fmt.Sprint(cells[idx["Type"]]); got != "Web" {
		t.Errorf("Type column = %q, want Web", got)
	}
	if got := fmt.Sprint(cells[idx["Ready"]]); got != "True" {
		t.Errorf("Ready column = %q, want True", got)
	}
	if got := fmt.Sprint(cells[idx["Replicas"]]); got != "1" {
		t.Errorf("Replicas column = %q, want 1", got)
	}
	// URL stays empty until the Domain reconciler derives it (next task).
	if got := cells[idx["URL"]]; got != nil && got != "" {
		t.Errorf("URL column = %v, want empty before any Domain exists", got)
	}
	if got := cells[idx["Age"]]; got == nil || got == "" {
		t.Errorf("Age column = %v, want a rendered age", got)
	}
}
