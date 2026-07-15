package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

const leaderElectionID = "orkano-operator.orkano.io"

// systemNamespace/appsNamespace/buildNamespace are defined in cache.go (the
// non-test file that owns CacheOptions), so the production scoping and the
// tests reference one set of namespace names.

var (
	k8sClient client.Client
	// cachedClient is the manager's cache-backed client — the one whose reads
	// are subject to CacheOptions scoping (k8sClient talks to the API server
	// directly and is unscoped).
	cachedClient client.Client
	restConfig   *rest.Config
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) (code int) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd"),
			// Stand-in cert-manager Certificate CRD: the Domain controller
			// watches Certificates, and tests play the part of ingress-shim.
			filepath.Join("..", "..", "..", "hack", "testdata", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start envtest: %v\n", err)
		return 1
	}
	restConfig = cfg
	defer func() {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to stop envtest: %v\n", err)
		}
	}()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(orkanov1alpha1.AddToScheme(scheme))

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		return 1
	}

	for _, name := range []string{systemNamespace, appsNamespace, buildNamespace} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := k8sClient.Create(context.Background(), ns); err != nil {
			fmt.Fprintf(os.Stderr, "failed to create %s namespace: %v\n", name, err)
			return 1
		}
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                        scheme,
		Cache:                         CacheOptions(),
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		LeaderElection:                true,
		LeaderElectionID:              leaderElectionID,
		LeaderElectionNamespace:       systemNamespace,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create manager: %v\n", err)
		return 1
	}
	cachedClient = mgr.GetClient()

	if err := (&AppReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up App controller: %v\n", err)
		return 1
	}
	if err := (&DomainReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(), ClusterIssuer: testClusterIssuer}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up Domain controller: %v\n", err)
		return 1
	}
	if err := (&RegistryCertReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up RegistryCert controller: %v\n", err)
		return 1
	}
	// The digest resolver is stubbed: envtest has no registry to HEAD. The
	// real Resolver's TLS round trip is covered by its own test against a
	// local TLS server (build_controller_test.go). GitBaseURL is a sentinel
	// (not the github.com default) so the Build-test context assertion proves
	// r.GitBaseURL threads through to the rendered Job, not a hardcoded prefix;
	// envtest never executes the build.
	if err := (&BuildReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), ResolveDigest: stubResolveDigest, GitBaseURL: "http://git.example.test/"}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up Build controller: %v\n", err)
		return 1
	}
	if err := (&PostgresReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up Postgres controller: %v\n", err)
		return 1
	}
	if err := (&MongoReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set up Mongo controller: %v\n", err)
		return 1
	}

	// Registered after the testEnv.Stop defer, so LIFO ordering joins the
	// manager (lease released against a live apiserver) before teardown.
	ctx, cancel := context.WithCancel(context.Background())
	mgrErr := make(chan error, 1)
	defer func() {
		cancel()
		if err := <-mgrErr; err != nil {
			fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}()
	go func() { mgrErr <- mgr.Start(ctx) }()

	syncCtx, syncCancel := context.WithTimeout(ctx, time.Minute)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		fmt.Fprintln(os.Stderr, "cache failed to sync")
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

func TestManagerAcquiresLeaderLease(t *testing.T) {
	key := types.NamespacedName{Namespace: systemNamespace, Name: leaderElectionID}
	eventually(t, "leader lease to be held", func(ctx context.Context) (bool, error) {
		var lease coordinationv1.Lease
		if err := k8sClient.Get(ctx, key, &lease); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "", nil
	})
}

func TestSchemeServesOrkanoKinds(t *testing.T) {
	ctx := context.Background()

	app := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "scheme-smoke", Namespace: "default"},
		Spec: orkanov1alpha1.AppSpec{
			Source: orkanov1alpha1.Source{
				GitHub: orkanov1alpha1.GitHubSource{Repo: "orkanoio/example"},
			},
			Build: orkanov1alpha1.BuildStrategy{Strategy: "Dockerfile"},
		},
	}
	if err := k8sClient.Create(ctx, app); err != nil {
		t.Fatalf("failed to create valid App: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, app) })

	var got orkanov1alpha1.App
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "scheme-smoke", Namespace: "default"}, &got); err != nil {
		t.Fatalf("failed to get App back: %v", err)
	}
	if got.Spec.Type != orkanov1alpha1.WorkloadWeb {
		t.Fatalf("schema default not applied: spec.type = %q, want %q", got.Spec.Type, orkanov1alpha1.WorkloadWeb)
	}

	for _, list := range []client.ObjectList{&orkanov1alpha1.BuildList{}, &orkanov1alpha1.DomainList{}, &orkanov1alpha1.PostgresList{}, &orkanov1alpha1.MongoList{}} {
		if err := k8sClient.List(ctx, list); err != nil {
			t.Fatalf("failed to list %T: %v", list, err)
		}
	}

	port := int32(8080)
	worker := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-worker", Namespace: "default"},
		Spec: orkanov1alpha1.AppSpec{
			Source: orkanov1alpha1.Source{
				GitHub: orkanov1alpha1.GitHubSource{Repo: "orkanoio/example"},
			},
			Build: orkanov1alpha1.BuildStrategy{Strategy: "Dockerfile"},
			Type:  orkanov1alpha1.WorkloadWorker,
			Port:  &port,
		},
	}
	err := k8sClient.Create(ctx, worker)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "Worker apps cannot set port or healthCheck") {
		t.Fatalf("expected the Worker CEL rule to reject, got: %v", err)
	}
}
