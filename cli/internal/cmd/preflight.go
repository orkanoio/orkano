package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/preflight/cluster"
)

type preflightOptions struct {
	kubeconfig string
	jsonOut    bool
}

func newPreflightCommand() *cobra.Command {
	opt := &preflightOptions{}
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Probe an existing cluster's capabilities before installing Orkano",
		Long: "Run the bring-your-own-cluster preflight against the cluster the kubeconfig " +
			"points at — the mandatory gate before installing Orkano onto a cluster you " +
			"already run (ADR-0019). Read-only checks cover the Kubernetes version window, " +
			"a default StorageClass, an IngressClass, and whether the kubeconfig identity " +
			"holds the RBAC the install needs; the capability probes then create short-lived " +
			"canary pods in generated scratch namespaces to prove the CNI enforces " +
			"NetworkPolicy, Pod Security Admission is active, and every build-eligible node " +
			"runs AppArmor-confined, seccomp-unconfined build canaries. Scratch namespaces " +
			"and canaries are deleted afterwards. Run it as the same identity that will " +
			"perform the install — the RBAC verdict answers for whoever the kubeconfig " +
			"authenticates. Exit codes gate CI: 0 all critical checks passed, 1 a " +
			"critical check failed, 2 a critical check could not be determined.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runClusterPreflight(cmd.Context(), cmd.OutOrStdout(), opt)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opt.kubeconfig, "kubeconfig", "", "path to the target cluster's kubeconfig (default: $KUBECONFIG, then ./orkano.kubeconfig)")
	f.BoolVar(&opt.jsonOut, "json", false, "emit the report as JSON")

	return cmd
}

// newPreflightCluster builds the cluster client plus the discovery-backed
// server-version reader the cluster checks need, from one kubeconfig parse. A
// package-var seam (the newDoctorClient idiom) so the command tests inject a
// fake cluster.
var newPreflightCluster = func(kubeconfigPath string) (ctrlclient.Client, func() (*version.Info, error), error) {
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load kubeconfig %s: %w", kubeconfigPath, err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, nil, fmt.Errorf("build scheme: %w", err)
	}
	c, err := ctrlclient.New(restCfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, fmt.Errorf("build cluster client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build discovery client: %w", err)
	}
	return c, disco.ServerVersion, nil
}

func runClusterPreflight(ctx context.Context, out io.Writer, opt *preflightOptions) error {
	kubeconfig := resolveKubeconfig(opt.kubeconfig, os.Getenv("KUBECONFIG"))
	c, serverVersion, err := newPreflightCluster(kubeconfig)
	if err != nil {
		return err
	}

	reg := checks.New()
	if err := cluster.Register(reg, cluster.Options{Client: c, ServerVersion: serverVersion}); err != nil {
		return fmt.Errorf("register cluster preflight checks: %w", err)
	}

	run, err := reg.Run(ctx)
	if err != nil {
		return err
	}

	if opt.jsonOut {
		err = run.WriteJSON(out)
	} else {
		err = run.WriteText(out)
	}
	if err != nil {
		return err
	}

	switch code := run.ExitCode(); code {
	case checks.ExitOK:
		return nil
	case checks.ExitCritical:
		return &exitCodeError{code: code, msg: "the cluster failed a preflight check — do not install until it is resolved (see the report above)"}
	default:
		return &exitCodeError{code: code, msg: "a preflight check could not be determined — do not install until it can (see the report above)"}
	}
}
