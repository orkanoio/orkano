package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/operator/internal/controller"
	"github.com/orkanoio/orkano/operator/internal/dispatcher"
	"github.com/orkanoio/orkano/operator/internal/githubapp"
	"github.com/orkanoio/orkano/operator/internal/registry"
)

// envDBDSN is the connection string for the webhook delivery queue, using the
// least-privilege orkano_dispatcher role. Empty disables the dispatcher (e.g.
// dev runs and envtest), so the rest of the operator still works without a DB.
const envDBDSN = "ORKANO_DB_DSN"

var version = "dev"

func main() {
	var (
		probeAddr               string
		leaderElectionNamespace string
		clusterIssuer           string
		githubBaseURL           string
		dispatchPollInterval    time.Duration
		maxConcurrentBuilds     int
	)
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the healthz/readyz endpoints bind to.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "orkano-system",
		"Namespace the leader-election Lease lives in.")
	flag.StringVar(&clusterIssuer, "cluster-issuer", "orkano-platform",
		"Name of the cert-manager ClusterIssuer every Domain Ingress is annotated with.")
	flag.StringVar(&githubBaseURL, "github-base-url", "",
		"GitHub API base URL the dispatcher re-fetches commits from; empty uses https://api.github.com (set for GitHub Enterprise).")
	flag.DurationVar(&dispatchPollInterval, "dispatch-poll-interval", dispatcher.DefaultPollInterval,
		"How often the webhook dispatcher polls the delivery queue.")
	flag.IntVar(&maxConcurrentBuilds, "max-concurrent-builds", dispatcher.DefaultMaxConcurrentBuilds,
		"Cap on in-flight Builds the dispatcher will create.")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("setup")
	log.Info("starting orkano-operator", "version", version)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(orkanov1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// Scope informers to Orkano's namespaces, per type, matching the
		// operator's namespaced RBAC — a cluster-wide cache would Forbid.
		Cache: controller.CacheOptions(),
		// No metrics endpoint until a task wires SecureServing + authn/authz
		// filters; a bind flag here would only ever enable plaintext HTTP.
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          true,
		LeaderElectionID:        "orkano-operator.orkano.io",
		LeaderElectionNamespace: leaderElectionNamespace,
		// Safe to release on shutdown: main exits right after Start returns,
		// so a stale process can never act on a released lease.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.AppReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up App controller")
		os.Exit(1)
	}
	if err := (&controller.DomainReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(), ClusterIssuer: clusterIssuer}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up Domain controller")
		os.Exit(1)
	}
	if err := (&controller.RegistryCertReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up RegistryCert controller")
		os.Exit(1)
	}
	resolver := &registry.Resolver{Reader: mgr.GetAPIReader()}
	if err := (&controller.BuildReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), ResolveDigest: resolver.ResolveDigest}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up Build controller")
		os.Exit(1)
	}
	if err := (&controller.PostgresReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up Postgres controller")
		os.Exit(1)
	}

	// The dispatcher consumes the webhook queue and creates Builds. It needs a
	// DB connection (the orkano_dispatcher role) and re-fetches commits from
	// GitHub via the App credentials the operator alone can read (INV-07). With
	// no DSN it stays off, so the operator runs without a queue (dev/envtest).
	if dsn := os.Getenv(envDBDSN); dsn != "" {
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			log.Error(err, "unable to create dispatcher DB pool")
			os.Exit(1)
		}
		defer pool.Close()
		if err := mgr.Add(&dispatcher.Dispatcher{
			Client:              mgr.GetClient(),
			Queue:               &dispatcher.PgxQueue{Pool: pool},
			GitHub:              &githubapp.TokenSource{Reader: mgr.GetAPIReader(), BaseURL: githubBaseURL},
			Log:                 ctrl.Log.WithName("dispatcher"),
			PollInterval:        dispatchPollInterval,
			MaxConcurrentBuilds: maxConcurrentBuilds,
		}); err != nil {
			log.Error(err, "unable to add dispatcher to manager")
			os.Exit(1)
		}
	} else {
		log.Info("dispatcher disabled: "+envDBDSN+" is not set", "env", envDBDSN)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}
