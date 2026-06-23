package install

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	// registryHost is the canonical, portless image host the build pipeline
	// pushes to and the kubelet pulls from — the single string form the registry
	// is ever addressed by (config/registry, INV-06's future VAP matches it).
	registryHost = "orkano-registry.orkano-system.svc.cluster.local"

	// registryTLSSecret is the cert-manager-issued registry TLS Secret whose
	// ca.crt is the trust root build pods and the node container runtime need.
	registryTLSSecret = "orkano-registry-tls"

	// registryService is the registry's Service; its ClusterIP is what the node
	// container runtime reaches (cluster DNS is unreachable from the host netns,
	// so /etc/hosts maps the registry host name to it).
	registryService = "orkano-registry"

	// buildsNS holds the build-pod CA projection ConfigMap.
	buildsNS = "orkano-builds"
	// registryCAConfigMap is the build-pod CA projection (orkano-builds): build
	// Jobs mount it at /orkano-registry-ca (config/buildkit, the Build template).
	registryCAConfigMap = "orkano-registry-ca"

	// nodeRegistriesPath and nodeRegistryCAPath are the node-side k3s registry
	// config and the CA it references. k3s reads both at startup and regenerates
	// containerd's registry config from them, so a change needs a k3s restart.
	nodeRegistriesPath = "/etc/rancher/k3s/registries.yaml"
	nodeRegistryCAPath = "/etc/rancher/k3s/orkano-registry-ca.crt"
	// nodeHostsPath maps the registry host name to its ClusterIP so the node's
	// host-netns container runtime can resolve it (no cluster DNS on the host).
	nodeHostsPath = "/etc/hosts"

	// nodeRegistriesMode keeps registries.yaml root-only; the CA is a public cert.
	nodeRegistriesMode = "0600"
	nodeRegistryCAMode = "0644"

	// registryHostsMarker tags the single /etc/hosts line WriteNodeRegistry
	// manages, so a re-run (or a ClusterIP change) replaces it in place and never
	// touches any other entry the node already has.
	registryHostsMarker = " # orkano-registry"
)

// defaultRestartReadyTimeout bounds the wait for a node's local apiserver to
// answer /readyz after a registries.yaml-triggered k3s restart, used when
// Config.RestartReadyTimeout is unset; a package var so tests can shrink it.
var defaultRestartReadyTimeout = 2 * time.Minute

// RegistryInfo carries the runtime registry facts the per-node write needs. They
// exist only after the registry is Ready (Apply waits for it), so they are read
// once on the first server and reused for every node.
type RegistryInfo struct {
	// CA is the issued internal registry CA (ca.crt) — the trust root the build
	// pods and every node's container runtime verify the registry against.
	CA []byte
	// ClusterIP is the registry Service's ClusterIP, mapped to the canonical
	// registry host name in each node's /etc/hosts.
	ClusterIP string
}

// WireRegistry runs the cluster-side half of registry wiring on the first
// server: it reads the issued registry CA and the registry Service's ClusterIP,
// publishes the build-pod CA projection ConfigMap (orkano-registry-ca in
// orkano-builds), and renders the node-ingress NetworkPolicy that lets the
// nodes' container runtimes reach the registry across the host network
// (orkano-registry-ingress-nodes — one /32 per node InternalIP, the companion
// to the static config/netpol ingress policy). It returns the CA + ClusterIP so
// the caller can write registries.yaml on every node. The registry must be
// Ready first (Apply waits for it) so the TLS Secret and ClusterIP exist.
func WireRegistry(ctx context.Context, r Runner, cfg Config) (*RegistryInfo, error) {
	if r == nil {
		return nil, errors.New("install: runner is required")
	}
	n := newNode(r, cfg.Sudo, cfg.Logf)

	ca, err := n.readRegistryCA(ctx)
	if err != nil {
		return nil, err
	}
	clusterIP, err := n.readRegistryClusterIP(ctx)
	if err != nil {
		return nil, err
	}
	nodeIPs, err := n.readNodeInternalIPs(ctx)
	if err != nil {
		return nil, err
	}

	if err := n.applyManifest(ctx, "registry CA ConfigMap", registryCAConfigMapManifest(ca)); err != nil {
		return nil, err
	}
	n.logf("published the registry CA to %s/%s", buildsNS, registryCAConfigMap)
	if err := n.applyManifest(ctx, "registry node-ingress policy", registryIngressNodesManifest(nodeIPs)); err != nil {
		return nil, err
	}
	n.logf("allowed %d node(s) to reach the registry", len(nodeIPs))

	return &RegistryInfo{CA: ca, ClusterIP: clusterIP}, nil
}

// WriteNodeRegistry points one node's container runtime at the in-cluster
// registry. It writes registries.yaml (trusting the internal CA for the registry
// host) and the CA file, and maps the registry host name to its ClusterIP in
// /etc/hosts — all idempotently. Resolving the host NAME (rather than mirroring
// to a bare IP) is deliberate: the container runtime then dials the name and TLS
// validates against the cert's DNS SAN, which an IP endpoint would fail (the
// registry cert carries no IP SAN). When registries.yaml or the CA changed it
// restarts k3s so containerd re-reads them, then waits for that node's local
// apiserver to answer /readyz before returning — so the HA caller can converge
// nodes one at a time without risking etcd quorum. The /etc/hosts mapping is
// read live by the resolver and needs no restart. It returns whether anything
// on the node changed.
func WriteNodeRegistry(ctx context.Context, r Runner, info *RegistryInfo, cfg Config) (bool, error) {
	if r == nil {
		return false, errors.New("install: runner is required")
	}
	// Defense in depth: the ClusterIP lands in /etc/hosts; re-validate it even
	// though the only in-repo producer (WireRegistry) already did (mirrors
	// validateTargets' posture).
	if info == nil || len(info.CA) == 0 || net.ParseIP(info.ClusterIP) == nil {
		return false, errors.New("install: registry info is incomplete")
	}
	n := newNode(r, cfg.Sudo, cfg.Logf)

	caChanged, err := n.ensureFile(ctx, nodeRegistryCAPath, info.CA, nodeRegistryCAMode)
	if err != nil {
		return false, err
	}
	cfgChanged, err := n.ensureFile(ctx, nodeRegistriesPath, registriesYAML(), nodeRegistriesMode)
	if err != nil {
		return false, err
	}
	hostsChanged, err := n.ensureHostsEntry(ctx, info.ClusterIP, registryHost)
	if err != nil {
		return false, err
	}
	changed := caChanged || cfgChanged || hostsChanged

	// Only registries.yaml/CA need a k3s restart (containerd re-reads them at
	// startup); a host-name mapping change is picked up live by the resolver.
	if !caChanged && !cfgChanged {
		return changed, nil
	}
	n.logf("restarting k3s to apply registries.yaml")
	if err := n.runOK(ctx, n.sudo+"systemctl restart k3s", "restart k3s"); err != nil {
		return true, err
	}
	if err := n.waitAPIReady(ctx, cfg.restartReadyTimeout()); err != nil {
		return true, err
	}
	return true, nil
}

// ensureHostsEntry maps host to ip in /etc/hosts, replacing the line this
// installer manages (tagged with registryHostsMarker) and preserving every other
// entry. It reports whether it wrote. /etc/hosts always exists; an unreadable one
// is an error rather than a silent overwrite that would clobber the file.
func (n *node) ensureHostsEntry(ctx context.Context, ip, host string) (bool, error) {
	cur, err := n.r.Run(ctx, n.sudo+"cat "+nodeHostsPath)
	if err != nil {
		return false, fmt.Errorf("install: read %s: %w", nodeHostsPath, err)
	}
	if cur.ExitStatus != 0 {
		return false, fmt.Errorf("install: %s is unreadable (exit %d)", nodeHostsPath, cur.ExitStatus)
	}
	desired := renderHosts(cur.Stdout, ip, host)
	if cur.Stdout == desired {
		return false, nil
	}
	enc := base64.StdEncoding.EncodeToString([]byte(desired))
	cmd := fmt.Sprintf("printf %%s '%s' | base64 -d | %stee %s >/dev/null", enc, n.sudo, nodeHostsPath)
	if err := n.runOK(ctx, cmd, "update "+nodeHostsPath); err != nil {
		return false, err
	}
	return true, nil
}

// renderHosts returns the /etc/hosts content with exactly one managed mapping of
// host to ip, preserving every other line. The managed line is marked so a
// re-run (or a ClusterIP change) replaces it in place rather than appending a
// duplicate. ip is IP-validated and host is a fixed constant, so neither can
// corrupt the file.
func renderHosts(current, ip, host string) string {
	var kept []string
	for _, line := range strings.Split(current, "\n") {
		if strings.HasSuffix(line, registryHostsMarker) {
			continue // drop our prior managed line
		}
		kept = append(kept, line)
	}
	body := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	return body + "\n" + ip + " " + host + registryHostsMarker + "\n"
}

// readRegistryCA reads the issued registry CA (ca.crt from the TLS Secret) and
// validates it is a PEM certificate before it lands on every node's filesystem.
func (n *node) readRegistryCA(ctx context.Context) ([]byte, error) {
	// The jsonpath is single-quoted so the escaped dot in `ca\.crt` and the braces
	// reach kubectl literally; the key is "ca.crt".
	out, err := n.kubectlOutput(ctx, fmt.Sprintf("get secret %s -n %s -o 'jsonpath={.data.ca\\.crt}'", registryTLSSecret, systemNS))
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, fmt.Errorf("install: registry CA (%s/%s ca.crt) is not yet issued", systemNS, registryTLSSecret)
	}
	ca, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		return nil, fmt.Errorf("install: decode registry CA: %w", err)
	}
	if block, _ := pem.Decode(ca); block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("install: registry CA is not a PEM certificate")
	}
	return ca, nil
}

// readRegistryClusterIP reads the registry Service's ClusterIP and validates it
// parses as an IP (it is mapped to the registry host name in each node's
// /etc/hosts — not written into registries.yaml; see WriteNodeRegistry).
func (n *node) readRegistryClusterIP(ctx context.Context) (string, error) {
	out, err := n.kubectlOutput(ctx, fmt.Sprintf("get svc %s -n %s -o 'jsonpath={.spec.clusterIP}'", registryService, systemNS))
	if err != nil {
		return "", err
	}
	if net.ParseIP(out) == nil {
		return "", fmt.Errorf("install: registry ClusterIP %q is not a valid IP", out)
	}
	return out, nil
}

// readNodeInternalIPs reads every node's InternalIP and validates each parses as
// an IP (they land in the node-ingress policy's /32 ipBlocks).
func (n *node) readNodeInternalIPs(ctx context.Context) ([]string, error) {
	out, err := n.kubectlOutput(ctx, `get nodes -o 'jsonpath={.items[*].status.addresses[?(@.type=="InternalIP")].address}'`)
	if err != nil {
		return nil, err
	}
	ips := strings.Fields(out)
	if len(ips) == 0 {
		return nil, errors.New("install: no node InternalIPs found")
	}
	for _, ip := range ips {
		if net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("install: node InternalIP %q is not a valid IP", ip)
		}
	}
	return ips, nil
}

// kubectlOutput runs a kubectl read and returns its trimmed stdout, erroring on
// a non-zero exit (an empty result on exit 0 is a valid "absent", handled by the
// caller — jsonpath of a missing field exits 0 with empty output).
func (n *node) kubectlOutput(ctx context.Context, args string) (string, error) {
	res, err := n.r.Run(ctx, fmt.Sprintf("%s%s kubectl %s", n.sudo, k3sBin, args))
	if err != nil {
		return "", fmt.Errorf("install: kubectl %s: %w", args, err)
	}
	if res.ExitStatus != 0 {
		return "", fmt.Errorf("install: kubectl %s exited %d: %s", args, res.ExitStatus, firstLine(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// applyManifest pipes a manifest into `kubectl apply -f -` base64-decoded (no
// content in argv) — idempotent, so a re-run or a CA rotation reconciles it.
func (n *node) applyManifest(ctx context.Context, desc string, manifest []byte) error {
	enc := base64.StdEncoding.EncodeToString(manifest)
	cmd := fmt.Sprintf("printf %%s '%s' | base64 -d | %s%s kubectl apply -f -", enc, n.sudo, k3sBin)
	return n.runOK(ctx, cmd, "apply "+desc)
}

// waitAPIReady polls the node's local apiserver /readyz until it returns ok or
// the timeout elapses; a transport error or a non-ok body (the apiserver is
// still coming up after the restart) means keep waiting.
func (n *node) waitAPIReady(ctx context.Context, timeout time.Duration) error {
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	for {
		res, err := n.r.Run(wait, fmt.Sprintf("%s%s kubectl get --raw=/readyz", n.sudo, k3sBin))
		if err == nil && res.ExitStatus == 0 && strings.TrimSpace(res.Stdout) == "ok" {
			return nil
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("install: k3s apiserver did not become ready within %s after restart", timeout)
		case <-ticker.C:
		}
	}
}

// registryCAConfigMapManifest renders the build-pod CA projection ConfigMap. The
// names are fixed constants and the CA is a validated PEM cert embedded in a
// literal block scalar, so the YAML cannot be injected.
func registryCAConfigMapManifest(ca []byte) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\ndata:\n  ca.crt: |\n", registryCAConfigMap, buildsNS)
	for _, line := range strings.Split(strings.TrimRight(string(ca), "\n"), "\n") {
		b.WriteString("    ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// registryIngressNodesManifest renders the companion node-ingress NetworkPolicy:
// one /32 ipBlock per node InternalIP allowed to the registry pod on 5000 (the
// post-DNAT port). Cross-node kubelet pulls originate from the host netns, which
// pod selectors don't match, so the static config/netpol policy can't express
// this — node IPs are install-specific (substrate smoke probe 9 rehearses it).
func registryIngressNodesManifest(nodeIPs []string) []byte {
	var b strings.Builder
	b.WriteString("apiVersion: networking.k8s.io/v1\n")
	b.WriteString("kind: NetworkPolicy\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: orkano-registry-ingress-nodes\n")
	b.WriteString("  namespace: " + systemNS + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  podSelector:\n")
	b.WriteString("    matchLabels:\n")
	b.WriteString("      app.kubernetes.io/name: orkano-registry\n")
	b.WriteString("  policyTypes: [Ingress]\n")
	b.WriteString("  ingress:\n")
	b.WriteString("    - ports:\n")
	b.WriteString("        - port: 5000\n")
	b.WriteString("          protocol: TCP\n")
	b.WriteString("      from:\n")
	for _, ip := range nodeIPs {
		b.WriteString("        - ipBlock:\n")
		b.WriteString("            cidr: " + ip + "/32\n")
	}
	return []byte(b.String())
}

// registriesYAML renders k3s's node-side registry config: it trusts the internal
// CA for the canonical registry host. There is deliberately no mirror/endpoint —
// the host name is resolved to the registry ClusterIP via /etc/hosts (written by
// WriteNodeRegistry), so the container runtime dials the host NAME and TLS
// validates against the cert's DNS SAN. A bare-IP endpoint would fail x509: the
// registry cert carries no IP SAN. This block only supplies the CA trust root,
// so it is identical on every node (the ClusterIP lives only in /etc/hosts).
func registriesYAML() []byte {
	return []byte(fmt.Sprintf(`configs:
  "%s":
    tls:
      ca_file: %s
`, registryHost, nodeRegistryCAPath))
}
