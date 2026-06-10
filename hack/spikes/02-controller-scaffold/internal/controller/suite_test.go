package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spikev1 "orkano-spike-controller/api/v1"
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to stop envtest: %v\n", err)
		}
	}()

	if err := spikev1.AddToScheme(scheme.Scheme); err != nil {
		fmt.Fprintf(os.Stderr, "failed to add scheme: %v\n", err)
		return 1
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create manager: %v\n", err)
		return 1
	}

	if err := (&AppSpikeReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up reconciler: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
		}
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		fmt.Fprintln(os.Stderr, "cache failed to sync")
		return 1
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		return 1
	}

	return m.Run()
}

func eventually(t *testing.T, desc string, cond func(ctx context.Context) (bool, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, cond); err != nil {
		t.Fatalf("timed out waiting for %s: %v", desc, err)
	}
}

func TestAppSpikeLifecycle(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Name: "spike-sample", Namespace: "default"}

	app := &spikev1.AppSpike{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec:       spikev1.AppSpikeSpec{Message: "hello from spike 2"},
	}
	if err := k8sClient.Create(ctx, app); err != nil {
		t.Fatalf("failed to create AppSpike: %v", err)
	}

	t.Run("finalizer added", func(t *testing.T) {
		eventually(t, "finalizer to appear", func(ctx context.Context) (bool, error) {
			var got spikev1.AppSpike
			if err := k8sClient.Get(ctx, key, &got); err != nil {
				return false, err
			}
			for _, f := range got.Finalizers {
				if f == FinalizerName {
					return true, nil
				}
			}
			return false, nil
		})
	})

	t.Run("ready condition with observedGeneration", func(t *testing.T) {
		eventually(t, "Ready condition", func(ctx context.Context) (bool, error) {
			var got spikev1.AppSpike
			if err := k8sClient.Get(ctx, key, &got); err != nil {
				return false, err
			}
			cond := meta.FindStatusCondition(got.Status.Conditions, "Ready")
			if cond == nil {
				return false, nil
			}
			ok := cond.Status == metav1.ConditionTrue &&
				cond.Reason == "MessageAccepted" &&
				cond.ObservedGeneration == got.Generation &&
				got.Status.ObservedGeneration == got.Generation
			return ok, nil
		})
	})

	t.Run("delete removes finalizer and object", func(t *testing.T) {
		if err := k8sClient.Delete(ctx, app); err != nil {
			t.Fatalf("failed to delete AppSpike: %v", err)
		}
		eventually(t, "object to be gone", func(ctx context.Context) (bool, error) {
			var got spikev1.AppSpike
			err := k8sClient.Get(ctx, key, &got)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
	})
}
