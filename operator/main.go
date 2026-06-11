package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

var version = "dev"

func main() {
	var (
		metricsAddr             string
		probeAddr               string
		leaderElectionNamespace string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0",
		`The address the metrics endpoint binds to; "0" disables it.`)
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the healthz/readyz endpoints bind to.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "orkano-system",
		"Namespace the leader-election Lease lives in.")
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
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
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
