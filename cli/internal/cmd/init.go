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
	"github.com/orkanoio/orkano/internal/nodeprep"
	"github.com/orkanoio/orkano/internal/preflight"
	"github.com/orkanoio/orkano/internal/ssh"
	"github.com/spf13/cobra"
)

type initOptions struct {
	nodes         []string
	sshUser       string
	sshPort       int
	sshKeyPath    string
	hostKeyPaths  []string
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
		Short: "Bootstrap a hardened k3s cluster on one or three nodes over SSH",
		Long: "Install a hardened, CIS-aligned k3s cluster on Linux nodes over SSH: run " +
			"the install preflight, write the hardening configuration (embedded etcd, " +
			"secrets encryption, audit logging, scheduled etcd snapshots), install k3s, " +
			"and retrieve a kubeconfig. Pass --node once for a single node or three " +
			"times for an HA cluster — the first node initialises the embedded-etcd " +
			"cluster and the rest join it. Safe to re-run — it converges every node.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opt)
		},
	}

	f := cmd.Flags()
	f.StringArrayVar(&opt.nodes, "node", nil, "node hostname or IPv4 address; repeat for an HA cluster, first is the cluster-init server (required)")
	f.StringVar(&opt.sshUser, "ssh-user", "root", "SSH user (non-root users need passwordless sudo)")
	f.IntVar(&opt.sshPort, "ssh-port", 22, "SSH port")
	f.StringVar(&opt.sshKeyPath, "ssh-key", "", "path to the SSH private key for authentication (required)")
	f.StringArrayVar(&opt.hostKeyPaths, "ssh-host-key", nil, "path to a node's known host public key (authorized-keys format) to pin; repeat once per --node, in order")
	f.BoolVar(&opt.acceptNewKey, "accept-new-host-key", false, "trust the host key presented on first contact (its fingerprint is printed)")
	f.StringVar(&opt.k3sVersion, "k3s-version", k3s.DefaultK3sVersion, "k3s version to install")
	f.StringVar(&opt.kubeconfig, "kubeconfig", "orkano.kubeconfig", "path to write the retrieved kubeconfig")
	f.DurationVar(&opt.readyTimeout, "ready-timeout", k3s.DefaultReadyTimeout, "how long to wait for the node to become Ready")
	f.BoolVar(&opt.skipPreflight, "skip-preflight", false, "skip the install preflight checks (not recommended)")

	return cmd
}

func runInit(ctx context.Context, out, errw io.Writer, opt *initOptions) error {
	if len(opt.nodes) == 0 {
		return fmt.Errorf("--node is required (repeat it for an HA cluster)")
	}
	if opt.sshKeyPath == "" {
		return fmt.Errorf("--ssh-key is required")
	}
	// v1 supports a single node or a 3-node embedded-etcd HA cluster (odd count
	// for etcd quorum); larger clusters are out of scope until tested.
	if n := len(opt.nodes); n != 1 && n != 3 {
		return fmt.Errorf("orkano init supports 1 or 3 servers (odd count for etcd quorum); got %d", n)
	}
	if dup := firstDuplicate(opt.nodes); dup != "" {
		return fmt.Errorf("--node %q given more than once", dup)
	}
	if len(opt.hostKeyPaths) > 0 && len(opt.hostKeyPaths) != len(opt.nodes) {
		return fmt.Errorf("--ssh-host-key must be given once per --node (%d nodes, %d host keys)", len(opt.nodes), len(opt.hostKeyPaths))
	}

	privateKey, err := os.ReadFile(opt.sshKeyPath)
	if err != nil {
		return fmt.Errorf("read SSH key: %w", err)
	}

	// The first server initialises the cluster; the rest join it at its API
	// endpoint with the token it generates. Every server's cert covers all node
	// addresses so the kubeconfig (and any load balancer placed in front) is
	// valid against any of them.
	var first *k3s.Result
	serverURL, token := "", ""
	anyChanged, anyFresh := false, false
	for i, node := range opt.nodes {
		hostKeyPath := ""
		if len(opt.hostKeyPaths) > 0 {
			hostKeyPath = opt.hostKeyPaths[i]
		}
		cfg := k3s.Config{
			NodeAddress:   node,
			ExtraTLSSANs:  otherNodes(opt.nodes, i),
			ServerURL:     serverURL,
			Token:         token,
			MinReadyNodes: i + 1,
			K3sVersion:    opt.k3sVersion,
			Sudo:          opt.sshUser != "root",
			ReadyTimeout:  opt.readyTimeout,
			Logf:          func(format string, args ...any) { writef(errw, "  "+format+"\n", args...) },
		}
		res, apparmorChanged, err := bootstrapOne(ctx, out, errw, opt, privateKey, node, hostKeyPath, cfg)
		if err != nil {
			return fmt.Errorf("bootstrap %s: %w", node, err)
		}
		anyChanged = anyChanged || res.Changed || apparmorChanged
		anyFresh = anyFresh || !res.AlreadyInstalled
		if i == 0 {
			first = res
			serverURL = fmt.Sprintf("https://%s:6443", node)
			token = res.Token
		}
	}

	if err := os.WriteFile(opt.kubeconfig, first.Kubeconfig, 0o600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}

	printSummary(out, opt, first, anyFresh, anyChanged)
	return nil
}

// bootstrapOne is the per-node bootstrap, indirected through a package variable
// so the HA-orchestration test can stub it and assert the loop's token/serverURL
// threading and kubeconfig selection without a live cluster. It returns the k3s
// result, whether the AppArmor node-prep step changed anything, and an error.
var bootstrapOne = bootstrapNode

// bootstrapNode runs the identical per-node setup (host-key pinning, preflight,
// k3s bootstrap, AppArmor profile load) so the HA loop does not duplicate it
// across servers. The second return reports whether the AppArmor step changed
// node state, so the summary headline reflects an AppArmor-only convergence.
func bootstrapNode(ctx context.Context, out, errw io.Writer, opt *initOptions, privateKey []byte, node, hostKeyPath string, cfg k3s.Config) (*k3s.Result, bool, error) {
	addr := net.JoinHostPort(node, fmt.Sprintf("%d", opt.sshPort))
	hostKey, err := resolveHostKey(ctx, errw, addr, hostKeyPath, opt.acceptNewKey)
	if err != nil {
		return nil, false, err
	}

	client, err := ssh.New(ssh.Config{
		Addr:       addr,
		User:       opt.sshUser,
		PrivateKey: privateKey,
		HostKey:    hostKey,
	})
	if err != nil {
		return nil, false, fmt.Errorf("configure SSH client for %s: %w", node, err)
	}
	defer func() { _ = client.Close() }()

	target := opt.sshUser + "@" + node
	if !opt.skipPreflight {
		if err := runPreflight(ctx, out, client, target); err != nil {
			return nil, false, err
		}
	}

	writef(errw, "Bootstrapping k3s on %s...\n", target)
	res, err := k3s.Bootstrap(ctx, client, cfg)
	if err != nil {
		return nil, false, err
	}

	// Load the build-confinement AppArmor profile on every node. Without it,
	// build pods scheduled on this node cannot start (ADR-0012), so it is
	// verified, not assumed; a failure refuses the node rather than leaving every
	// future build silently broken.
	writef(errw, "Loading build confinement profile on %s...\n", target)
	npRes, err := nodeprep.EnsureAppArmorProfile(ctx, nodeprep.Options{
		Runner: client,
		Sudo:   cfg.Sudo,
		Logf:   func(format string, args ...any) { writef(errw, "  "+format+"\n", args...) },
	})
	if err != nil {
		return nil, false, fmt.Errorf("load AppArmor profile on %s: %w", node, err)
	}
	return res, npRes.Changed, nil
}

// otherNodes returns every node address except the one at index skip.
func otherNodes(nodes []string, skip int) []string {
	others := make([]string, 0, len(nodes)-1)
	for i, n := range nodes {
		if i != skip {
			others = append(others, n)
		}
	}
	return others
}

// firstDuplicate returns the first repeated value in nodes, or "" if all unique.
func firstDuplicate(nodes []string) string {
	seen := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if _, ok := seen[n]; ok {
			return n
		}
		seen[n] = struct{}{}
	}
	return ""
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

// printSummary reports the cluster state. The headline verb reflects the
// aggregate across all servers (anyFresh/anyChanged) so a re-run that only
// converged a joiner does not falsely report "up to date"; per-node progress is
// streamed to stderr while bootstrapping. res carries the first server's
// representative encryption/audit/version state.
func printSummary(out io.Writer, opt *initOptions, res *k3s.Result, anyFresh, anyChanged bool) {
	first := opt.nodes[0]
	writef(out, "\n")
	switch {
	case anyFresh:
		writef(out, "Installed k3s %s on %s.\n", res.Version, first)
	case anyChanged:
		writef(out, "Converged k3s %s on %s.\n", res.Version, first)
	default:
		writef(out, "k3s %s on %s is already up to date.\n", res.Version, first)
	}
	writef(out, "  secrets encryption: %s\n", res.SecretsEncryption)
	writef(out, "  audit logging:      %s\n", presentLabel(res.AuditLogPresent))
	writef(out, "  build confinement:  AppArmor %s (enforce)\n", nodeprep.ProfileName)
	if len(opt.nodes) > 1 {
		writef(out, "  servers:            %d (HA, embedded etcd)\n", len(opt.nodes))
	}
	writef(out, "  kubeconfig:         %s\n", opt.kubeconfig)
	if len(opt.nodes) > 1 {
		writef(out, "\nThe kubeconfig points at the first server (%s). The cluster keeps serving\n"+
			"if another server fails, but API access through this kubeconfig needs the\n"+
			"first server reachable — for failure-tolerant access put a load balancer in\n"+
			"front of all servers and re-point the kubeconfig at it.\n", first)
	}
	writef(out, "\nTry it: KUBECONFIG=%s kubectl get nodes\n", opt.kubeconfig)
}

func presentLabel(present bool) string {
	if present {
		return "enabled"
	}
	return "not found"
}
