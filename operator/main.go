package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
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
	"github.com/orkanoio/orkano/internal/db"
	"github.com/orkanoio/orkano/internal/features"
	"github.com/orkanoio/orkano/internal/sourcearchive"
	"github.com/orkanoio/orkano/operator/internal/buildjob"
	"github.com/orkanoio/orkano/operator/internal/controller"
	"github.com/orkanoio/orkano/operator/internal/dispatcher"
	"github.com/orkanoio/orkano/operator/internal/githubapp"
	"github.com/orkanoio/orkano/operator/internal/gitresolver"
	"github.com/orkanoio/orkano/operator/internal/registry"
	"github.com/orkanoio/orkano/operator/internal/sourcefetch"
)

// envDBDSN is the connection string for the webhook delivery queue, using the
// least-privilege orkano_dispatcher role. Empty disables the dispatcher (e.g.
// dev runs and envtest), so the rest of the operator still works without a DB.
const envDBDSN = "ORKANO_DB_DSN"

const (
	envUnsafeFeatures     = "ORKANO_UNSAFE_FEATURES"
	envSourceFetcherImage = "ORKANO_SOURCE_FETCHER_IMAGE"
	defaultRegistryCAFile = "/orkano-registry-ca/ca.crt"
)

// The migrate subcommand reads the superuser DSN from envDBDSN and the
// install-generated passwords for the least-privilege roles from these (the
// names, not the credentials, so no gosec G101 suppression is needed).
const (
	envReceiverPassword   = "ORKANO_RECEIVER_PASSWORD"
	envDispatcherPassword = "ORKANO_DISPATCHER_PASSWORD"
	envDashboardPassword  = "ORKANO_DASHBOARD_PASSWORD"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "source-fetch" {
		if err := runSourceFetch(context.Background(), os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "source-fetch:", err)
			os.Exit(1)
		}
		return
	}
	// `orkano-operator migrate` is the one-shot entrypoint the platform's
	// migration Job runs at install: apply the schema and set the role
	// passwords with the superuser DSN, then exit. The long-running operator
	// never takes this path, so it is handled before any manager setup.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := runMigrate(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			os.Exit(1)
		}
		fmt.Println("migrations applied and role passwords set")
		return
	}

	runOperator()
}

func runSourceFetch(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("source-fetch", flag.ContinueOnError)
	var appName, digest, destination, registryURL, caFile string
	flags.StringVar(&appName, "app", "", "App that owns the source archive")
	flags.StringVar(&digest, "digest", "", "Expected sha256 source archive digest")
	flags.StringVar(&destination, "destination", "/workspace/source", "Empty directory to extract into")
	flags.StringVar(&registryURL, "registry-url", sourcearchive.DefaultRegistryURL, "Internal source registry URL")
	flags.StringVar(&caFile, "ca-file", defaultRegistryCAFile, "Registry CA certificate")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if appName == "" || digest == "" {
		return errors.New("--app and --digest are required")
	}
	registryClient, err := sourcearchive.NewTLSRegistry(registryURL, caFile)
	if err != nil {
		return err
	}
	return sourcefetch.Fetch(ctx, registryClient, sourcefetch.Config{AppName: appName, Digest: digest, Destination: destination})
}

// runMigrate applies the platform schema and assigns the least-privilege role
// passwords. It connects with the superuser DSN (envDBDSN here carries the
// superuser, not the dispatcher role the running operator uses) and the
// install-generated role passwords.
func runMigrate(ctx context.Context) error {
	dsn := os.Getenv(envDBDSN)
	if dsn == "" {
		return fmt.Errorf("%s is required", envDBDSN)
	}
	recvPw := os.Getenv(envReceiverPassword)
	if recvPw == "" {
		return fmt.Errorf("%s is required", envReceiverPassword)
	}
	dispPw := os.Getenv(envDispatcherPassword)
	if dispPw == "" {
		return fmt.Errorf("%s is required", envDispatcherPassword)
	}
	dashPw := os.Getenv(envDashboardPassword)
	if dashPw == "" {
		return fmt.Errorf("%s is required", envDashboardPassword)
	}
	if err := db.Migrate(ctx, dsn); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	if err := db.SetupRoles(ctx, dsn, db.RolePasswords{Receiver: recvPw, Dispatcher: dispPw, Dashboard: dashPw}); err != nil {
		return fmt.Errorf("set role passwords: %w", err)
	}
	return nil
}

func runOperator() {
	var (
		probeAddr               string
		leaderElectionNamespace string
		clusterIssuer           string
		githubBaseURL           string
		gitBaseURL              string
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
	flag.StringVar(&gitBaseURL, "git-base-url", buildjob.DefaultGitBaseURL,
		"Base URL builds clone the App's repo from; must end with '/'. Override for an in-cluster git server (hermetic E2E) or an air-gapped mirror.")
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
	featureSet, err := features.ParseCSV(os.Getenv(envUnsafeFeatures))
	if err != nil {
		log.Error(err, "invalid unsafe feature configuration", "env", envUnsafeFeatures)
		os.Exit(1)
	}
	sourceFetcherImage := strings.TrimSpace(os.Getenv(envSourceFetcherImage))
	if featureSet.Enabled(features.SourceZip) && sourceFetcherImage == "" {
		log.Error(errors.New("source fetcher image is required when source.zip is enabled"), "invalid unsafe feature configuration", "env", envSourceFetcherImage)
		os.Exit(1)
	}

	// Compose appends "<owner>/<name>.git#<commit>" straight onto the base, so a
	// missing trailing slash silently malforms every git context. Fail fast.
	if !strings.HasSuffix(gitBaseURL, "/") {
		log.Error(fmt.Errorf("--git-base-url=%q must end with '/'", gitBaseURL), "invalid flag")
		os.Exit(1)
	}

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
	if err := (&controller.BuildReconciler{
		Client:             mgr.GetClient(),
		APIReader:          mgr.GetAPIReader(),
		ResolveDigest:      resolver.ResolveDigest,
		GitBaseURL:         gitBaseURL,
		SourceFetcherImage: sourceFetcherImage,
		Features:           featureSet,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up Build controller")
		os.Exit(1)
	}
	if err := (&controller.PostgresReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up Postgres controller")
		os.Exit(1)
	}
	if err := (&controller.MongoReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up Mongo controller")
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
			Git:                 gitresolver.New(),
			Features:            featureSet,
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
