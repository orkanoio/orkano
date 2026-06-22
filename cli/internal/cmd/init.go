package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/k3s"
	"github.com/orkanoio/orkano/internal/preflight"
	"github.com/orkanoio/orkano/internal/ssh"
	"github.com/spf13/cobra"
)

type initOptions struct {
	node          string
	sshUser       string
	sshPort       int
	sshKeyPath    string
	hostKeyPath   string
	acceptNewKey  bool
	k3sVersion    string
	kubeconfig    string
	readyTimeout  time.Duration
	skipPreflight bool
}

func newInitCommand() *cobra.Command {
	opt := &initOptions{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Bootstrap a hardened k3s cluster on a node over SSH",
		Long: "Install a hardened, CIS-aligned single-node k3s cluster on a Linux node " +
			"over SSH: run the install preflight, write the hardening configuration " +
			"(embedded etcd, secrets encryption, audit logging), install k3s, and " +
			"retrieve a kubeconfig. Safe to re-run — it converges the node.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opt)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opt.node, "node", "", "node hostname or IPv4 address to install onto (required)")
	f.StringVar(&opt.sshUser, "ssh-user", "root", "SSH user (non-root users need passwordless sudo)")
	f.IntVar(&opt.sshPort, "ssh-port", 22, "SSH port")
	f.StringVar(&opt.sshKeyPath, "ssh-key", "", "path to the SSH private key for authentication (required)")
	f.StringVar(&opt.hostKeyPath, "ssh-host-key", "", "path to the node's known host public key (authorized-keys format) to pin")
	f.BoolVar(&opt.acceptNewKey, "accept-new-host-key", false, "trust the host key presented on first contact (its fingerprint is printed)")
	f.StringVar(&opt.k3sVersion, "k3s-version", k3s.DefaultK3sVersion, "k3s version to install")
	f.StringVar(&opt.kubeconfig, "kubeconfig", "orkano.kubeconfig", "path to write the retrieved kubeconfig")
	f.DurationVar(&opt.readyTimeout, "ready-timeout", k3s.DefaultReadyTimeout, "how long to wait for the node to become Ready")
	f.BoolVar(&opt.skipPreflight, "skip-preflight", false, "skip the install preflight checks (not recommended)")

	return cmd
}

func runInit(ctx context.Context, out, errw io.Writer, opt *initOptions) error {
	if opt.node == "" {
		return fmt.Errorf("--node is required")
	}
	if opt.sshKeyPath == "" {
		return fmt.Errorf("--ssh-key is required")
	}

	privateKey, err := os.ReadFile(opt.sshKeyPath)
	if err != nil {
		return fmt.Errorf("read SSH key: %w", err)
	}

	addr := net.JoinHostPort(opt.node, fmt.Sprintf("%d", opt.sshPort))
	hostKey, err := resolveHostKey(ctx, errw, addr, opt.hostKeyPath, opt.acceptNewKey)
	if err != nil {
		return err
	}

	client, err := ssh.New(ssh.Config{
		Addr:       addr,
		User:       opt.sshUser,
		PrivateKey: privateKey,
		HostKey:    hostKey,
	})
	if err != nil {
		return fmt.Errorf("configure SSH client: %w", err)
	}
	defer func() { _ = client.Close() }()

	target := opt.sshUser + "@" + opt.node
	if !opt.skipPreflight {
		if err := runPreflight(ctx, out, client, target); err != nil {
			return err
		}
	}

	writef(errw, "Bootstrapping k3s on %s...\n", target)
	res, err := k3s.Bootstrap(ctx, client, k3s.Config{
		NodeAddress:  opt.node,
		K3sVersion:   opt.k3sVersion,
		Sudo:         opt.sshUser != "root",
		ReadyTimeout: opt.readyTimeout,
		Logf:         func(format string, args ...any) { writef(errw, "  "+format+"\n", args...) },
	})
	if err != nil {
		return err
	}

	if err := os.WriteFile(opt.kubeconfig, res.Kubeconfig, 0o600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}

	printSummary(out, opt, res)
	return nil
}

// resolveHostKey returns the host public key to pin: the explicit file if given,
// otherwise the key the node presents on first contact — but only when the user
// opted in with --accept-new-host-key, after its fingerprint is shown.
func resolveHostKey(ctx context.Context, errw io.Writer, addr, hostKeyPath string, acceptNew bool) ([]byte, error) {
	if hostKeyPath != "" {
		//nolint:gosec // G304: the path is a user-supplied flag; reading the named file is the point.
		key, err := os.ReadFile(hostKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read host key: %w", err)
		}
		return key, nil
	}

	scanned, err := ssh.ScanHostKey(ctx, addr, ssh.DefaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("scan host key: %w", err)
	}
	fp, err := ssh.FingerprintSHA256(scanned)
	if err != nil {
		return nil, fmt.Errorf("fingerprint host key: %w", err)
	}
	if !acceptNew {
		return nil, fmt.Errorf("host key for %s is not trusted (fingerprint %s)\n"+
			"verify it out of band, then re-run with --accept-new-host-key, or pass --ssh-host-key <file>", addr, fp)
	}
	writef(errw, "Trusting host key for %s (fingerprint %s)\n", addr, fp)
	return scanned, nil
}

// writef writes best-effort progress and summary text. A failure to write to the
// terminal is not actionable in a CLI, so the error is deliberately ignored.
func writef(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// runPreflight executes the install preflight and refuses the install on any
// critical failure or indeterminate critical check.
func runPreflight(ctx context.Context, out io.Writer, exec preflight.Executor, target string) error {
	reg := checks.New()
	if err := preflight.Register(reg, preflight.Options{Executor: exec, Target: target}); err != nil {
		return fmt.Errorf("register preflight checks: %w", err)
	}
	run, err := reg.Run(ctx)
	if err != nil {
		return fmt.Errorf("run preflight: %w", err)
	}
	if err := run.WriteText(out); err != nil {
		return err
	}
	if !run.OK() {
		return fmt.Errorf("preflight failed; refusing to touch %s (re-run after fixing, or --skip-preflight to override)", target)
	}
	return nil
}

func printSummary(out io.Writer, opt *initOptions, res *k3s.Result) {
	writef(out, "\n")
	switch {
	case !res.AlreadyInstalled:
		writef(out, "Installed k3s %s on %s.\n", res.Version, opt.node)
	case res.Changed:
		writef(out, "Converged k3s %s on %s.\n", res.Version, opt.node)
	default:
		writef(out, "k3s %s on %s is already up to date.\n", res.Version, opt.node)
	}
	writef(out, "  secrets encryption: %s\n", res.SecretsEncryption)
	writef(out, "  audit logging:      %s\n", presentLabel(res.AuditLogPresent))
	writef(out, "  kubeconfig:         %s\n\n", opt.kubeconfig)
	writef(out, "Try it: KUBECONFIG=%s kubectl get nodes\n", opt.kubeconfig)
}

func presentLabel(present bool) string {
	if present {
		return "enabled"
	}
	return "not found"
}
