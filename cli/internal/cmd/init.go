package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/install"
	"github.com/orkanoio/orkano/internal/k3s"
	"github.com/orkanoio/orkano/internal/localexec"
	"github.com/orkanoio/orkano/internal/nodeprep"
	"github.com/orkanoio/orkano/internal/preflight"
	"github.com/orkanoio/orkano/internal/ssh"
	"github.com/spf13/cobra"
)

type initOptions struct {
	version       string
	local         bool
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
	acmeEmail     string
	acmeProd      bool
	allowRepos    []string
	receiverHost  string
	secretsVault  bool
}

func newInitCommand(version string) *cobra.Command {
	opt := &initOptions{version: version}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Bootstrap a hardened k3s cluster over SSH, or on this machine with --local",
		Long: "Install a hardened, CIS-aligned k3s cluster on Linux nodes over SSH: run " +
			"the install preflight, write the hardening configuration (embedded etcd, " +
			"secrets encryption, audit logging, scheduled etcd snapshots), install k3s, " +
			"and retrieve a kubeconfig. Pass --node once for a single node or three " +
			"times for an HA cluster — the first node initialises the embedded-etcd " +
			"cluster and the rest join it. Use --local to install on the machine you " +
			"are running on instead (single node, no SSH) — the get.orkano.io one-liner. " +
			"Safe to re-run — it converges every node.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opt)
		},
	}

	f := cmd.Flags()
	f.BoolVar(&opt.local, "local", false, "install on THIS machine directly (single-node, no SSH); must run as root")
	f.StringArrayVar(&opt.nodes, "node", nil, "node hostname or IPv4 address; repeat for an HA cluster, first is the cluster-init server (required unless --local)")
	f.StringVar(&opt.sshUser, "ssh-user", "root", "SSH user (non-root users need passwordless sudo)")
	f.IntVar(&opt.sshPort, "ssh-port", 22, "SSH port")
	f.StringVar(&opt.sshKeyPath, "ssh-key", "", "path to the SSH private key for authentication (required)")
	f.StringArrayVar(&opt.hostKeyPaths, "ssh-host-key", nil, "path to a node's known host public key (authorized-keys format) to pin; repeat once per --node, in order")
	f.BoolVar(&opt.acceptNewKey, "accept-new-host-key", false, "trust the host key presented on first contact (its fingerprint is printed)")
	f.StringVar(&opt.k3sVersion, "k3s-version", k3s.DefaultK3sVersion, "k3s version to install")
	f.StringVar(&opt.kubeconfig, "kubeconfig", "orkano.kubeconfig", "path to write the retrieved kubeconfig")
	f.DurationVar(&opt.readyTimeout, "ready-timeout", k3s.DefaultReadyTimeout, "how long to wait for the node to become Ready")
	f.BoolVar(&opt.skipPreflight, "skip-preflight", false, "skip the install preflight checks (not recommended)")
	f.StringVar(&opt.acmeEmail, "acme-email", "", "email to register the Let's Encrypt account with (optional)")
	f.BoolVar(&opt.acmeProd, "acme-prod", false, "use Let's Encrypt production instead of staging (staging is the safe default)")
	f.StringArrayVar(&opt.allowRepos, "allow-repo", nil, "owner/repo allowed to trigger builds; repeat to allow several (the webhook receiver's allowlist)")
	f.StringVar(&opt.receiverHost, "receiver-host", "", "public hostname to expose the webhook receiver on over HTTPS (optional; without it the receiver stays cluster-internal)")
	f.BoolVar(&opt.secretsVault, "secrets-vault", false, "install the External Secrets Operator for external secret stores (Vault etc.); opt-in — re-run init with this flag to add it later")

	return cmd
}

func runInit(ctx context.Context, out, errw io.Writer, opt *initOptions) error {
	if opt.local {
		return runInitLocal(ctx, out, errw, opt)
	}
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
	// One pinned host key per node, kept so the component deploy (first server)
	// and the registry wiring (every node) can reconnect without re-scanning.
	hostKeys := make([][]byte, len(opt.nodes))
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
		res, hostKey, apparmorChanged, err := bootstrapOne(ctx, out, errw, opt, privateKey, node, hostKeyPath, cfg)
		if err != nil {
			return fmt.Errorf("bootstrap %s: %w", node, err)
		}
		hostKeys[i] = hostKey
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

	// Deploy the platform components onto the now-running cluster (CRDs,
	// cert-manager, RBAC, NetworkPolicies, registry, platform Postgres, operator,
	// receiver) and the generated Secrets, then wait for the critical path.
	writef(errw, "Deploying Orkano components on %s...\n", opt.nodes[0])
	bootstrapToken, err := deployComponents(ctx, out, errw, opt, privateKey, hostKeys[0])
	if err != nil {
		printBootstrapTokenAfterFailure(out, bootstrapToken)
		return fmt.Errorf("deploy components: %w", err)
	}

	// Point every node's container runtime at the now-issued in-cluster registry
	// (registries.yaml + CA, a k3s restart per changed node) and publish the
	// build-pod CA + the node-ingress policy from the first server.
	writef(errw, "Wiring the in-cluster registry...\n")
	if err := wireRegistry(ctx, out, errw, opt, privateKey, hostKeys); err != nil {
		// The deploy step succeeded, so a fresh token exists and printSummary —
		// its only other outlet — is never reached. Losing it here is the same
		// lockout as losing it on the deploy failure above.
		printBootstrapTokenAfterFailure(out, bootstrapToken)
		return fmt.Errorf("wire registry: %w", err)
	}

	printSummary(out, opt, first, anyFresh, anyChanged, bootstrapToken)
	return nil
}

// localRunner is the transport `orkano init --local` runs the bootstrap engine
// over: the same Run(ctx, cmd) (ssh.Result, error) contract as *ssh.Client,
// satisfied by *localexec.Runner. Threading it through k3s.Bootstrap /
// install.Apply / nodeprep unchanged is the whole point of ADR-0017 — one
// engine, a swapped transport.
type localRunner interface {
	Run(ctx context.Context, cmd string) (ssh.Result, error)
}

// Package-var seams so the CLI-orchestration tests can inject a fake node and
// stub the component deploy / registry wiring (both engine-tested in
// internal/install), mirroring the SSH path's bootstrapOne/deployComponents/
// wireRegistry. The k3s bootstrap + AppArmor prep run inline against the
// injected runner, so the happy-path test exercises the real transport threading
// (as the SSH happy path does over sshtest).
var (
	geteuid           = os.Geteuid
	localAddress      = detectPrimaryIP
	newLocalRunner    = func() localRunner { return localexec.New() }
	deployLocal       = deployComponentsLocal
	wireRegistryLocal = wireRegistryLocalNode
)

// runInitLocal installs a hardened single-node cluster on the machine running
// the command, over a local-exec runner instead of SSH (ADR-0017). It reuses the
// entire bootstrap engine (preflight, k3s, AppArmor, component deploy, registry
// wiring) unchanged — only the transport differs. Single-node only: HA needs the
// other servers reached over SSH, which stays the --node path.
func runInitLocal(ctx context.Context, out, errw io.Writer, opt *initOptions) error {
	if len(opt.nodes) > 1 {
		return fmt.Errorf("--local installs a single node on this machine; got %d --node values (use SSH --node for HA)", len(opt.nodes))
	}
	// ADR-0017: --local takes NO --ssh-* flags. --ssh-user/--ssh-port always carry
	// their flag defaults ("root"/22), so only a non-default value marks explicit
	// use — a silently ignored flag would let the user believe it took effect.
	if opt.sshKeyPath != "" || len(opt.hostKeyPaths) > 0 || opt.acceptNewKey ||
		(opt.sshUser != "" && opt.sshUser != "root") || (opt.sshPort != 0 && opt.sshPort != 22) {
		return fmt.Errorf("--local runs on this machine and takes no SSH flags (--ssh-key/--ssh-host-key/--accept-new-host-key/--ssh-user/--ssh-port)")
	}
	// k3s install and the AppArmor profile load both need root; refuse cleanly
	// rather than failing mid-install. The install.sh wrapper runs the binary
	// under sudo, so a piped one-liner still works.
	if geteuid() != 0 {
		return fmt.Errorf("orkano init --local must run as root on this machine (re-run with sudo)")
	}

	addr := ""
	if len(opt.nodes) == 1 {
		addr = opt.nodes[0]
	} else {
		a, err := localAddress()
		if err != nil {
			return fmt.Errorf("could not determine this machine's address; pass --node <address>: %w", err)
		}
		addr = a
	}

	runner := newLocalRunner()
	logf := func(format string, args ...any) { writef(errw, "  "+format+"\n", args...) }

	if !opt.skipPreflight {
		// Sudo false: --local requires root, so the readyz probe runs directly.
		if err := runPreflight(ctx, out, runner, "this machine ("+addr+")", false); err != nil {
			return err
		}
	}

	writef(errw, "Bootstrapping k3s on this machine (%s)...\n", addr)
	// Sudo stays false: --local requires root, so commands run directly.
	res, err := k3s.Bootstrap(ctx, runner, k3s.Config{
		NodeAddress:   addr,
		MinReadyNodes: 1,
		K3sVersion:    opt.k3sVersion,
		ReadyTimeout:  opt.readyTimeout,
		Logf:          logf,
	})
	if err != nil {
		return fmt.Errorf("bootstrap this machine: %w", err)
	}

	writef(errw, "Loading build confinement profile...\n")
	npRes, err := nodeprep.EnsureAppArmorProfile(ctx, nodeprep.Options{Runner: runner, Logf: logf})
	if err != nil {
		return fmt.Errorf("load AppArmor profile: %w", err)
	}

	if err := os.WriteFile(opt.kubeconfig, res.Kubeconfig, 0o600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}

	writef(errw, "Deploying Orkano components...\n")
	bootstrapToken, err := deployLocal(ctx, errw, opt, runner)
	if err != nil {
		printBootstrapTokenAfterFailure(out, bootstrapToken)
		return fmt.Errorf("deploy components: %w", err)
	}

	writef(errw, "Wiring the in-cluster registry...\n")
	if err := wireRegistryLocal(ctx, errw, opt, runner, addr); err != nil {
		// Same as the SSH path: a fresh token from the successful deploy step
		// would otherwise be lost with the skipped summary.
		printBootstrapTokenAfterFailure(out, bootstrapToken)
		return fmt.Errorf("wire registry: %w", err)
	}

	printLocalSummary(out, opt, res, !res.AlreadyInstalled, res.Changed || npRes.Changed, bootstrapToken, addr)
	return nil
}

// readinessTargets is the critical path the component deploy waits for,
// extended with the External Secrets Operator's when it is opted in.
func readinessTargets(opt *initOptions) []install.Workload {
	targets := install.DefaultReadinessTargets()
	if opt.secretsVault {
		targets = append(targets, install.SecretsVaultReadinessTargets()...)
	}
	return targets
}

// deployComponentsLocal runs the component deploy over the local runner. It is
// wrapped by the deployLocal package var so tests stub it like deployComponents.
func deployComponentsLocal(ctx context.Context, errw io.Writer, opt *initOptions, runner localRunner) (string, error) {
	res, err := install.Apply(ctx, runner, install.Config{
		Version:          opt.version,
		ACMEEmail:        opt.acmeEmail,
		ACMEProd:         opt.acmeProd,
		RepoAllowlist:    opt.allowRepos,
		ReceiverHost:     opt.receiverHost,
		SecretsVault:     opt.secretsVault,
		ReadinessTargets: readinessTargets(opt),
		Logf:             func(format string, args ...any) { writef(errw, "  "+format+"\n", args...) },
	})
	if err != nil {
		if res != nil {
			return res.BootstrapToken, err
		}
		return "", err
	}
	return res.BootstrapToken, nil
}

// wireRegistryLocalNode wires the in-cluster registry on this single node over
// the local runner. It is wrapped by the wireRegistryLocal package var for tests.
func wireRegistryLocalNode(ctx context.Context, errw io.Writer, opt *initOptions, runner localRunner, addr string) error {
	cfg := install.Config{
		RestartReadyTimeout: opt.readyTimeout,
		Logf:                func(format string, args ...any) { writef(errw, "  "+format+"\n", args...) },
	}
	info, err := install.WireRegistry(ctx, runner, cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", addr, err)
	}
	if _, err := install.WriteNodeRegistry(ctx, runner, info, cfg); err != nil {
		return fmt.Errorf("%s: %w", addr, err)
	}
	return nil
}

// detectPrimaryIP returns the IP of the interface carrying the default route, by
// opening (not sending on) a UDP socket to a documentation address and reading
// the source the kernel selects. It errors when there is no route — an isolated
// host must pass --node <address> to advertise.
func detectPrimaryIP() (string, error) {
	// 192.0.2.1 is TEST-NET-1 (RFC 5737); a UDP "dial" only consults the routing
	// table to pick a source address — no packet is sent to it, so a background
	// context is right (the call does not block on the network).
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "udp", "192.0.2.1:9")
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return "", err
	}
	return host, nil
}

// deployComponents is the component-deploy step, indirected through a package
// variable so the CLI-orchestration tests can stub it (like bootstrapOne) and
// assert what runInit passes without a live cluster. It returns the one-time
// bootstrap token (empty on a re-run where it already existed).
var deployComponents = deployOnNode0

// deployOnNode0 opens a fresh SSH connection to the first server and runs the
// component deploy over it. It reuses the host key pinned during bootstrap (no
// re-scan, no re-printed fingerprint). The first server is always a k3s server
// with the auto-deploy directory; the deploy is idempotent, so it is re-runnable.
func deployOnNode0(ctx context.Context, _, errw io.Writer, opt *initOptions, privateKey, hostKey []byte) (string, error) {
	node := opt.nodes[0]
	client, err := dialNode(opt, privateKey, node, hostKey)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()

	res, err := install.Apply(ctx, client, install.Config{
		Version:          opt.version,
		ACMEEmail:        opt.acmeEmail,
		ACMEProd:         opt.acmeProd,
		RepoAllowlist:    opt.allowRepos,
		ReceiverHost:     opt.receiverHost,
		SecretsVault:     opt.secretsVault,
		ReadinessTargets: readinessTargets(opt),
		Sudo:             opt.sshUser != "root",
		Logf:             func(format string, args ...any) { writef(errw, "  "+format+"\n", args...) },
	})
	if err != nil {
		if res != nil {
			return res.BootstrapToken, err
		}
		return "", err
	}
	return res.BootstrapToken, nil
}

// wireRegistry is the node-registry-wiring step, indirected through a package
// variable so the CLI-orchestration tests can stub it (like deployComponents).
// The wiring itself is engine-tested in internal/install over a fake node.
var wireRegistry = wireRegistryOnNodes

// wireRegistryOnNodes reads the issued registry CA + ClusterIP on the first
// server (publishing the build-pod CA ConfigMap and the node-ingress policy),
// then writes registries.yaml + the CA onto every node so the kubelet can pull
// the digest-pinned app images, restarting k3s where the config changed. Each
// connection reuses the host key pinned during bootstrap (no re-scan).
func wireRegistryOnNodes(ctx context.Context, _, errw io.Writer, opt *initOptions, privateKey []byte, hostKeys [][]byte) error {
	cfg := install.Config{
		Sudo:                opt.sshUser != "root",
		RestartReadyTimeout: opt.readyTimeout,
		Logf:                func(format string, args ...any) { writef(errw, "  "+format+"\n", args...) },
	}

	node0, err := dialNode(opt, privateKey, opt.nodes[0], hostKeys[0])
	if err != nil {
		return err
	}
	info, err := install.WireRegistry(ctx, node0, cfg)
	_ = node0.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", opt.nodes[0], err)
	}

	for i, node := range opt.nodes {
		client, err := dialNode(opt, privateKey, node, hostKeys[i])
		if err != nil {
			return err
		}
		writef(errw, "  configuring the registry on %s...\n", node)
		changed, err := install.WriteNodeRegistry(ctx, client, info, cfg)
		_ = client.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", node, err)
		}
		if changed {
			writef(errw, "  applied registry config on %s\n", node)
		}
	}
	return nil
}

// dialNode opens an SSH client to a node, pinning the host key resolved during
// bootstrap (no re-scan, no re-printed fingerprint).
func dialNode(opt *initOptions, privateKey []byte, node string, hostKey []byte) (*ssh.Client, error) {
	addr := net.JoinHostPort(node, fmt.Sprintf("%d", opt.sshPort))
	client, err := ssh.New(ssh.Config{Addr: addr, User: opt.sshUser, PrivateKey: privateKey, HostKey: hostKey})
	if err != nil {
		return nil, fmt.Errorf("configure SSH client for %s: %w", node, err)
	}
	return client, nil
}

// bootstrapOne is the per-node bootstrap, indirected through a package variable
// so the HA-orchestration test can stub it and assert the loop's token/serverURL
// threading and kubeconfig selection without a live cluster. It returns the k3s
// result, the resolved (pinned) host key, whether the AppArmor node-prep step
// changed anything, and an error.
var bootstrapOne = bootstrapNode

// bootstrapNode runs the identical per-node setup (host-key pinning, preflight,
// k3s bootstrap, AppArmor profile load) so the HA loop does not duplicate it
// across servers. It returns the resolved host key so the caller can reuse it
// for the component deploy on the first server without re-scanning (which would
// re-print the fingerprint), and whether the AppArmor step changed node state
// (so the summary headline reflects an AppArmor-only convergence).
func bootstrapNode(ctx context.Context, out, errw io.Writer, opt *initOptions, privateKey []byte, node, hostKeyPath string, cfg k3s.Config) (*k3s.Result, []byte, bool, error) {
	addr := net.JoinHostPort(node, fmt.Sprintf("%d", opt.sshPort))
	hostKey, err := resolveHostKey(ctx, errw, addr, hostKeyPath, opt.acceptNewKey)
	if err != nil {
		return nil, nil, false, err
	}

	client, err := ssh.New(ssh.Config{
		Addr:       addr,
		User:       opt.sshUser,
		PrivateKey: privateKey,
		HostKey:    hostKey,
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("configure SSH client for %s: %w", node, err)
	}
	defer func() { _ = client.Close() }()

	target := opt.sshUser + "@" + node
	if !opt.skipPreflight {
		if err := runPreflight(ctx, out, client, target, cfg.Sudo); err != nil {
			return nil, nil, false, err
		}
	}

	writef(errw, "Bootstrapping k3s on %s...\n", target)
	res, err := k3s.Bootstrap(ctx, client, cfg)
	if err != nil {
		return nil, nil, false, err
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
		return nil, nil, false, fmt.Errorf("load AppArmor profile on %s: %w", node, err)
	}
	return res, hostKey, npRes.Changed, nil
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
func runPreflight(ctx context.Context, out io.Writer, exec preflight.Executor, target string, sudo bool) error {
	reg := checks.New()
	if err := preflight.Register(reg, preflight.Options{Executor: exec, Target: target, AllowExistingK3s: true, Sudo: sudo}); err != nil {
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
func printSummary(out io.Writer, opt *initOptions, res *k3s.Result, anyFresh, anyChanged bool, bootstrapToken string) {
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
	writef(out, "  components:         deployed\n")
	writef(out, "  registry:           wired on every node\n")
	if opt.secretsVault {
		writef(out, "  secrets vault:      External Secrets Operator deployed (connect a store from the dashboard)\n")
	}
	if opt.receiverHost != "" {
		writef(out, "  receiver:           https://%s\n", opt.receiverHost)
	} else {
		writef(out, "  receiver:           cluster-internal (set --receiver-host to expose it)\n")
	}
	writef(out, "  kubeconfig:         %s\n", opt.kubeconfig)
	if len(opt.nodes) > 1 {
		writef(out, "\nThe kubeconfig points at the first server (%s). The cluster keeps serving\n"+
			"if another server fails, but API access through this kubeconfig needs the\n"+
			"first server reachable — for failure-tolerant access put a load balancer in\n"+
			"front of all servers and re-point the kubeconfig at it.\n", first)
	}
	// ADR-0003: the install token is printed exactly once. On a re-run it has
	// already been generated (only its hash is stored), so there is nothing to show.
	if bootstrapToken != "" {
		writef(out, "\nBootstrap token (shown once — store it now):\n  %s\n"+
			"Redeem it at first dashboard login to create the admin account (Phase 2).\n", bootstrapToken)
	} else {
		// The SSH-path reader is at their workstation, where the kubeconfig this
		// run just wrote already reaches the cluster (the "Try it" line below) —
		// no node-picking, no sudo, and it works uniformly across HA members.
		printBootstrapTokenRecovery(out, "KUBECONFIG=\""+opt.kubeconfig+"\" kubectl")
	}
	writef(out, "\nTry it: KUBECONFIG=%s kubectl get nodes\n", opt.kubeconfig)
}

func presentLabel(present bool) string {
	if present {
		return "enabled"
	}
	return "not found"
}

// printLocalSummary reports an on-box (--local) install: the same hardening state
// as printSummary, plus the private way in — the dashboard is ClusterIP-only and
// never exposed to the internet (INV-05), so the reach step is an SSH tunnel that
// port-forwards it to localhost, not a public URL (ADR-0017). It also states the
// single-node HA tradeoff honestly.
func printLocalSummary(out io.Writer, opt *initOptions, res *k3s.Result, fresh, changed bool, bootstrapToken, addr string) {
	writef(out, "\n")
	switch {
	case fresh:
		writef(out, "Installed k3s %s on this machine (%s).\n", res.Version, addr)
	case changed:
		writef(out, "Converged k3s %s on this machine (%s).\n", res.Version, addr)
	default:
		writef(out, "k3s %s on this machine (%s) is already up to date.\n", res.Version, addr)
	}
	writef(out, "  secrets encryption: %s\n", res.SecretsEncryption)
	writef(out, "  audit logging:      %s\n", presentLabel(res.AuditLogPresent))
	writef(out, "  build confinement:  AppArmor %s (enforce)\n", nodeprep.ProfileName)
	writef(out, "  nodes:              1 (single node — not highly available)\n")
	writef(out, "  components:         deployed\n")
	writef(out, "  registry:           wired\n")
	if opt.secretsVault {
		writef(out, "  secrets vault:      External Secrets Operator deployed (connect a store from the dashboard)\n")
	}
	if opt.receiverHost != "" {
		writef(out, "  receiver:           https://%s\n", opt.receiverHost)
	} else {
		writef(out, "  receiver:           cluster-internal (set --receiver-host to expose it)\n")
	}
	writef(out, "  kubeconfig:         %s\n", opt.kubeconfig)

	// ADR-0003: the install token is printed exactly once. On a re-run it has
	// already been generated (only its hash is stored), so there is nothing to show.
	if bootstrapToken != "" {
		writef(out, "\nBootstrap token (shown once — store it now):\n  %s\n"+
			"Redeem it at first dashboard login to create the admin account.\n", bootstrapToken)
	} else {
		// --local runs as root on the box itself, so the node's own k3s kubectl
		// is the reader's working command (absolute path: RHEL sudo secure_path).
		printBootstrapTokenRecovery(out, "/usr/local/bin/k3s kubectl")
	}

	// The dashboard is ClusterIP-only and never exposed to the internet (INV-05).
	// Reach it privately over an SSH tunnel that port-forwards the Service; a
	// one-command `orkano proxy` will replace this (ADR-0004).
	// The k3s path is absolute for the same reason internal/k3s uses it: RHEL-family
	// sudo secure_path (and a non-interactive sshd PATH) may exclude /usr/local/bin.
	writef(out, "\nReach the dashboard — it is private, never exposed to the internet.\n"+
		"From your laptop, open one SSH tunnel that also port-forwards it:\n\n"+
		"  ssh -L 9090:127.0.0.1:9090 root@%s \\\n"+
		"    '/usr/local/bin/k3s kubectl -n orkano-system port-forward --address 127.0.0.1 svc/orkano-dashboard 9090:80'\n\n"+
		"then open http://localhost:9090 and redeem the bootstrap token above.\n"+
		"(Connecting as a non-root user? Prefix the remote command with sudo.)\n", addr)

	writef(out, "\nThis is a single node — if it fails, the cluster is down. For high\n"+
		"availability, install three servers over SSH instead:\n"+
		"  orkano init --node A --node B --node C --ssh-key <key>\n")
}

func printBootstrapTokenAfterFailure(out io.Writer, bootstrapToken string) {
	if bootstrapToken == "" {
		return
	}
	writef(out, "\nBootstrap token (shown once — store it now):\n  %s\n"+
		"The install hit an error after creating this token. Re-run `orkano init` after fixing the error;\n"+
		"this token will not be shown again, but it will work once the dashboard is ready.\n", bootstrapToken)
}

// printBootstrapTokenRecovery prints the rotate-the-secret recipe for a re-run
// whose token was generated (and possibly never seen) on an earlier run. kubectl
// is the caller's working command base — the SSH path passes a KUBECONFIG-scoped
// kubectl for the workstation, --local the node's own k3s kubectl — so the
// recipe is copy-pasteable exactly where the reader is sitting. The digest goes
// through openssl (already required for the rand step) so the recipe works on
// macOS workstations too, which ship no sha256sum.
func printBootstrapTokenRecovery(out io.Writer, kubectl string) {
	writef(out, "\nBootstrap token already generated on a previous run (not shown again).\n"+
		"If that first run failed before you copied it, the plaintext token cannot be recovered because\n"+
		"only its sha256 hash is stored. Rotate it:\n\n"+
		"  TOKEN=$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')\n"+
		"  HASH=$(printf %%s \"$TOKEN\" | openssl dgst -sha256 | awk '{print $NF}')\n"+
		"  %s -n orkano-system create secret generic orkano-bootstrap-token \\\n"+
		"    --from-literal=token-sha256=\"$HASH\" --dry-run=client -o yaml | %s apply -f -\n"+
		"  %s -n orkano-system rollout restart deploy/orkano-dashboard\n"+
		"  printf 'Bootstrap token: %%s\\n' \"$TOKEN\"\n", kubectl, kubectl, kubectl)
}
