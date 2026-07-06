package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/doctor"
	"github.com/orkanoio/orkano/internal/nodeprep"
)

type doctorOptions struct {
	kubeconfig string
	jsonOut    bool
	fix        bool
	local      bool
}

func newDoctorCommand() *cobra.Command {
	opt := &doctorOptions{}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check a running Orkano install's health and hardening",
		Long: "Run the doctor checks against a live cluster and report each one with a " +
			"hardening score: a severity-weighted percentage of the applicable checks " +
			"that passed. Reads the cluster through the kubeconfig `orkano init` wrote; " +
			"the NetworkPolicy check additionally creates three short-lived canary pods " +
			"in orkano-builds to probe enforcement for real, and removes them after. " +
			"Pass --local when running on the server itself (as root) to also verify " +
			"on-box node state such as the AppArmor build confinement. Exit codes gate " +
			"CI: 0 all critical checks passed, 1 a critical check failed, 2 a critical " +
			"check could not be determined.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.Context(), cmd.OutOrStdout(), opt)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opt.kubeconfig, "kubeconfig", "", "path to the cluster kubeconfig (default: $KUBECONFIG, then ./orkano.kubeconfig)")
	f.BoolVar(&opt.jsonOut, "json", false, "emit the report as JSON")
	f.BoolVar(&opt.fix, "fix", false, "apply each failing check's automatic fix, then re-run every check")
	f.BoolVar(&opt.local, "local", false, "also run on-box node checks (AppArmor build confinement); must run on the server, as root")

	return cmd
}

// Package-var seams (the bootstrapOne idiom) so the command tests can inject a
// fake cluster client and extend the check set (e.g. with a fixable check to
// drive the --fix path).
var registerDoctorChecks = doctor.Register

// newDoctorClient builds the cluster client doctor reads through.
var newDoctorClient = func(kubeconfigPath string) (ctrlclient.Client, error) {
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %s: %w", kubeconfigPath, err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("build scheme: %w", err)
	}
	return ctrlclient.New(restCfg, ctrlclient.Options{Scheme: scheme})
}

func runDoctor(ctx context.Context, out io.Writer, opt *doctorOptions) error {
	// Reading the kernel's profile list under securityfs needs root; refuse
	// up front like `init --local` does — a non-root run would only produce an
	// indeterminate probe error whose remediation text ("run orkano init")
	// points the operator at the wrong problem.
	if opt.local && geteuid() != 0 {
		return fmt.Errorf("orkano doctor --local must run as root on the server (re-run with sudo)")
	}

	kubeconfig := resolveKubeconfig(opt.kubeconfig, os.Getenv("KUBECONFIG"))
	c, err := newDoctorClient(kubeconfig)
	if err != nil {
		return err
	}

	reg := checks.New()
	if err := registerDoctorChecks(reg, doctor.Options{Client: c}); err != nil {
		return fmt.Errorf("register doctor checks: %w", err)
	}
	if opt.local {
		if err := reg.Register(nodeprep.AppArmorProfileLoadedCheck(newLocalRunner(), false)); err != nil {
			return fmt.Errorf("register node checks: %w", err)
		}
	}

	var (
		run      *checks.Run
		attempts []checks.FixAttempt
	)
	if opt.fix {
		run, attempts, err = reg.RunAndFix(ctx)
	} else {
		run, err = reg.Run(ctx)
	}
	if err != nil {
		return err
	}

	if opt.jsonOut {
		err = doctor.WriteJSON(out, run, attempts)
	} else {
		err = doctor.WriteText(out, run, attempts)
	}
	if err != nil {
		return err
	}

	switch code := run.ExitCode(); code {
	case checks.ExitOK:
		return nil
	case checks.ExitCritical:
		return &exitCodeError{code: code, msg: "a critical check failed (see the report above)"}
	default:
		return &exitCodeError{code: code, msg: "a critical check could not be determined (see the report above)"}
	}
}

// resolveKubeconfig picks the kubeconfig path: the explicit flag, then
// $KUBECONFIG, then the ./orkano.kubeconfig `orkano init` writes.
func resolveKubeconfig(flag, env string) string {
	if flag != "" {
		return flag
	}
	if env != "" {
		return env
	}
	return "orkano.kubeconfig"
}

// exitCodeError carries a specific process exit code from a command to main —
// doctor's CI contract distinguishes a critical failure (1) from an
// indeterminate one (2), which a bare error cannot.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

// ExitCode maps a command error to the process exit code main should use: 0
// for nil, the carried code when the command gates with a specific one, and 1
// for every other error.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.code
	}
	return 1
}
