package install

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/orkanoio/orkano/internal/ssh"
	"sigs.k8s.io/yaml"
)

// testCAPEM is a PEM-framed CERTIFICATE block (readRegistryCA validates the PEM
// framing + type, not the DER, so a placeholder body is enough).
var testCAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("orkano-internal-ca-der")})

// fakeRegistryNode answers the registry-wiring commands: the kubectl reads for
// the CA/ClusterIP/node IPs, the ConfigMap/NetworkPolicy applies, the
// registries.yaml + CA + /etc/hosts writes, the k3s restart, and the
// post-restart /readyz poll. State drives the readiness wait and the read-failure
// cases, and lets a test seed existing files for the idempotent paths.
type fakeRegistryNode struct {
	files   map[string]string
	cmds    []string
	applied map[string]string // metadata.name -> applied manifest

	caB64     string // base64 of ca.crt to serve (empty = not issued yet)
	clusterIP string
	nodeIPs   string // space-joined InternalIPs

	failSecret bool // `get secret` exits non-zero (RBAC denied / apiserver down)
	failSvc    bool
	failNodes  bool

	restarted         bool
	readyAfterRestart int // /readyz polls that report not-ready before "ok"
	readyPolls        int
}

func newFakeRegistryNode() *fakeRegistryNode {
	return &fakeRegistryNode{
		// /etc/hosts always exists on a real node; seed the standard entry so the
		// hosts read in ensureHostsEntry succeeds.
		files:     map[string]string{nodeHostsPath: "127.0.0.1 localhost\n"},
		applied:   map[string]string{},
		caB64:     base64.StdEncoding.EncodeToString(testCAPEM),
		clusterIP: "10.43.0.42",
		nodeIPs:   "192.168.1.10",
	}
}

func (f *fakeRegistryNode) Run(_ context.Context, raw string) (ssh.Result, error) {
	f.cmds = append(f.cmds, raw)
	cmd := strings.ReplaceAll(raw, "sudo ", "")

	switch {
	case strings.Contains(cmd, "get --raw=/readyz"):
		if !f.restarted {
			return ssh.Result{Stderr: "connection refused", ExitStatus: 1}, nil
		}
		f.readyPolls++
		if f.readyPolls > f.readyAfterRestart {
			return ssh.Result{Stdout: "ok"}, nil
		}
		return ssh.Result{Stderr: "apiserver not ready", ExitStatus: 1}, nil

	case strings.Contains(cmd, "| base64 -d |") && strings.Contains(cmd, "kubectl apply -f -"):
		name, manifest := parseSecretApply(cmd) // reused: extracts metadata.name
		f.applied[name] = manifest
		return ssh.Result{}, nil

	case strings.Contains(cmd, "| base64 -d |"):
		p, c, appendMode := parseWrite(cmd)
		if appendMode {
			f.files[p] += c
		} else {
			f.files[p] = c
		}
		return ssh.Result{}, nil

	case strings.HasPrefix(cmd, "cat "):
		p := strings.TrimPrefix(cmd, "cat ")
		if c, ok := f.files[p]; ok {
			return ssh.Result{Stdout: c}, nil
		}
		return ssh.Result{Stderr: "No such file or directory", ExitStatus: 1}, nil

	case strings.Contains(cmd, "get secret "):
		if f.failSecret {
			return ssh.Result{Stderr: "Error from server (Forbidden)", ExitStatus: 1}, nil
		}
		return ssh.Result{Stdout: f.caB64}, nil
	case strings.Contains(cmd, "get svc "):
		if f.failSvc {
			return ssh.Result{Stderr: "Error from server (NotFound)", ExitStatus: 1}, nil
		}
		return ssh.Result{Stdout: f.clusterIP}, nil
	case strings.Contains(cmd, "get nodes "):
		if f.failNodes {
			return ssh.Result{Stderr: "The connection to the server was refused", ExitStatus: 1}, nil
		}
		return ssh.Result{Stdout: f.nodeIPs}, nil

	case strings.Contains(cmd, "systemctl restart k3s"):
		f.restarted = true
		return ssh.Result{}, nil

	default: // chmod, mkdir, mv, …
		return ssh.Result{}, nil
	}
}

func shrinkRestartTiming(t *testing.T) {
	t.Helper()
	prevPoll, prevTimeout := waitPollInterval, defaultRestartReadyTimeout
	waitPollInterval = time.Millisecond
	defaultRestartReadyTimeout = time.Second
	t.Cleanup(func() { waitPollInterval, defaultRestartReadyTimeout = prevPoll, prevTimeout })
}

func TestWireRegistryPublishesCAAndPolicy(t *testing.T) {
	n := newFakeRegistryNode()
	n.nodeIPs = "192.168.1.10 192.168.1.11 192.168.1.12"

	info, err := WireRegistry(context.Background(), n, Config{})
	if err != nil {
		t.Fatalf("WireRegistry: %v", err)
	}
	if string(info.CA) != string(testCAPEM) {
		t.Error("returned CA does not match the issued cert")
	}
	if info.ClusterIP != "10.43.0.42" {
		t.Errorf("ClusterIP = %q, want 10.43.0.42", info.ClusterIP)
	}

	cm, ok := n.applied["orkano-registry-ca"]
	if !ok {
		t.Fatal("registry CA ConfigMap was not applied")
	}
	assertValidYAML(t, "registry-ca-configmap", []byte(cm))
	// Round-trip the block scalar so a future indentation regression fails loudly
	// rather than passing a substring check.
	var parsed struct {
		Kind     string                     `json:"kind"`
		Metadata struct{ Namespace string } `json:"metadata"`
		Data     map[string]string          `json:"data"`
	}
	if err := yaml.Unmarshal([]byte(cm), &parsed); err != nil {
		t.Fatalf("CA ConfigMap not parseable: %v", err)
	}
	if parsed.Kind != "ConfigMap" || parsed.Metadata.Namespace != "orkano-builds" {
		t.Errorf("CA ConfigMap wrong kind/namespace: %+v", parsed)
	}
	if parsed.Data["ca.crt"] != string(testCAPEM) {
		t.Errorf("CA ConfigMap ca.crt did not round-trip:\n got %q\nwant %q", parsed.Data["ca.crt"], testCAPEM)
	}

	np, ok := n.applied["orkano-registry-ingress-nodes"]
	if !ok {
		t.Fatal("node-ingress NetworkPolicy was not applied")
	}
	assertValidYAML(t, "registry-ingress-nodes", []byte(np))
	for _, want := range []string{
		"kind: NetworkPolicy",
		"namespace: orkano-system",
		"app.kubernetes.io/name: orkano-registry",
		"port: 5000",
		"cidr: 192.168.1.10/32",
		"cidr: 192.168.1.11/32",
		"cidr: 192.168.1.12/32",
	} {
		if !strings.Contains(np, want) {
			t.Errorf("node-ingress policy missing %q:\n%s", want, np)
		}
	}
}

func TestWireRegistryRejectsBadInputs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeRegistryNode)
	}{
		{"CA not issued yet", func(f *fakeRegistryNode) { f.caB64 = "" }},
		{"CA not a PEM cert", func(f *fakeRegistryNode) {
			f.caB64 = base64.StdEncoding.EncodeToString([]byte("not pem at all"))
		}},
		{"ClusterIP not an IP", func(f *fakeRegistryNode) { f.clusterIP = "not-an-ip" }},
		{"no node IPs", func(f *fakeRegistryNode) { f.nodeIPs = "" }},
		{"bad node IP", func(f *fakeRegistryNode) { f.nodeIPs = "192.168.1.10 garbage" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeRegistryNode()
			tc.setup(n)
			if _, err := WireRegistry(context.Background(), n, Config{}); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestWireRegistrySurfacesReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeRegistryNode)
	}{
		{"CA read denied", func(f *fakeRegistryNode) { f.failSecret = true }},
		{"ClusterIP read fails", func(f *fakeRegistryNode) { f.failSvc = true }},
		{"node list fails", func(f *fakeRegistryNode) { f.failNodes = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newFakeRegistryNode()
			tc.setup(n)
			_, err := WireRegistry(context.Background(), n, Config{})
			if err == nil || !strings.Contains(err.Error(), "kubectl") {
				t.Fatalf("want a surfaced kubectl read error, got %v", err)
			}
		})
	}
}

func TestWireRegistryNilRunner(t *testing.T) {
	if _, err := WireRegistry(context.Background(), nil, Config{}); err == nil {
		t.Fatal("expected an error for a nil runner")
	}
}

func TestWriteNodeRegistryWritesAndRestarts(t *testing.T) {
	shrinkRestartTiming(t)
	n := newFakeRegistryNode()
	info := &RegistryInfo{CA: testCAPEM, ClusterIP: "10.43.0.42"}

	changed, err := WriteNodeRegistry(context.Background(), n, info, Config{})
	if err != nil {
		t.Fatalf("WriteNodeRegistry: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on a fresh node")
	}

	reg, ok := n.files[nodeRegistriesPath]
	if !ok {
		t.Fatal("registries.yaml was not written")
	}
	assertValidYAML(t, "registries.yaml", []byte(reg))
	for _, want := range []string{
		`"orkano-registry.orkano-system.svc.cluster.local"`,
		"ca_file: /etc/rancher/k3s/orkano-registry-ca.crt",
	} {
		if !strings.Contains(reg, want) {
			t.Errorf("registries.yaml missing %q:\n%s", want, reg)
		}
	}
	// A bare-IP endpoint is exactly the x509 trap this approach avoids.
	if strings.Contains(reg, "endpoint") || strings.Contains(reg, "10.43.0.42") {
		t.Errorf("registries.yaml must not mirror to the ClusterIP (no IP SAN on the cert):\n%s", reg)
	}
	if n.files[nodeRegistryCAPath] != string(testCAPEM) {
		t.Error("CA file not written with the issued cert")
	}
	// The ClusterIP is mapped in /etc/hosts so the host resolves the registry name.
	if hosts := n.files[nodeHostsPath]; !strings.Contains(hosts, "10.43.0.42 orkano-registry.orkano-system.svc.cluster.local") {
		t.Errorf("/etc/hosts missing the registry mapping:\n%s", hosts)
	} else if !strings.Contains(hosts, "127.0.0.1 localhost") {
		t.Errorf("/etc/hosts clobbered the existing entries:\n%s", hosts)
	}
	if !n.restarted {
		t.Error("k3s was not restarted to apply the new config")
	}
	if !hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, "get --raw=/readyz") }) {
		t.Error("did not wait for the apiserver to come back after the restart")
	}
}

func TestWriteNodeRegistryIdempotent(t *testing.T) {
	shrinkRestartTiming(t)
	n := newFakeRegistryNode()
	info := &RegistryInfo{CA: testCAPEM, ClusterIP: "10.43.0.42"}
	// Pre-seed exactly what WriteNodeRegistry would write.
	n.files[nodeRegistryCAPath] = string(testCAPEM)
	n.files[nodeRegistriesPath] = string(registriesYAML())
	n.files[nodeHostsPath] = renderHosts("127.0.0.1 localhost\n", "10.43.0.42", registryHost)

	changed, err := WriteNodeRegistry(context.Background(), n, info, Config{})
	if err != nil {
		t.Fatalf("WriteNodeRegistry: %v", err)
	}
	if changed {
		t.Error("expected changed=false when everything already matches")
	}
	if n.restarted {
		t.Error("k3s must not restart when nothing changed")
	}
}

// TestWriteNodeRegistryPartialChange covers the realistic re-run cases the
// restart gate hinges on: a CA rotation (CA file differs, registries.yaml same)
// and a registries.yaml change both must restart; a /etc/hosts-only change (a
// ClusterIP move) must NOT restart because the resolver reads it live.
func TestWriteNodeRegistryPartialChange(t *testing.T) {
	info := &RegistryInfo{CA: testCAPEM, ClusterIP: "10.43.0.42"}
	converged := func() *fakeRegistryNode {
		n := newFakeRegistryNode()
		n.files[nodeRegistryCAPath] = string(testCAPEM)
		n.files[nodeRegistriesPath] = string(registriesYAML())
		n.files[nodeHostsPath] = renderHosts("127.0.0.1 localhost\n", "10.43.0.42", registryHost)
		return n
	}

	t.Run("CA rotated", func(t *testing.T) {
		shrinkRestartTiming(t)
		n := converged()
		n.files[nodeRegistryCAPath] = "-----BEGIN CERTIFICATE-----\nstale\n-----END CERTIFICATE-----\n"
		changed, err := WriteNodeRegistry(context.Background(), n, info, Config{})
		if err != nil || !changed || !n.restarted {
			t.Fatalf("CA change must rewrite + restart: changed=%v restarted=%v err=%v", changed, n.restarted, err)
		}
	})

	t.Run("registries.yaml changed", func(t *testing.T) {
		shrinkRestartTiming(t)
		n := converged()
		n.files[nodeRegistriesPath] = "configs: {}\n"
		changed, err := WriteNodeRegistry(context.Background(), n, info, Config{})
		if err != nil || !changed || !n.restarted {
			t.Fatalf("registries.yaml change must rewrite + restart: changed=%v restarted=%v err=%v", changed, n.restarted, err)
		}
	})

	t.Run("hosts only changed", func(t *testing.T) {
		shrinkRestartTiming(t)
		n := converged()
		// The ClusterIP moved: only /etc/hosts differs.
		n.files[nodeHostsPath] = renderHosts("127.0.0.1 localhost\n", "10.43.0.99", registryHost)
		changed, err := WriteNodeRegistry(context.Background(), n, info, Config{})
		if err != nil {
			t.Fatalf("WriteNodeRegistry: %v", err)
		}
		if !changed {
			t.Error("a /etc/hosts mapping change should report changed=true")
		}
		if n.restarted {
			t.Error("a /etc/hosts-only change must NOT restart k3s (the resolver reads it live)")
		}
		if !strings.Contains(n.files[nodeHostsPath], "10.43.0.42 "+registryHost) {
			t.Error("the stale ClusterIP mapping was not replaced")
		}
	})
}

func TestWriteNodeRegistryRestartReadyTransient(t *testing.T) {
	shrinkRestartTiming(t)
	n := newFakeRegistryNode()
	n.readyAfterRestart = 2 // /readyz is not-ready for two polls, then ok
	info := &RegistryInfo{CA: testCAPEM, ClusterIP: "10.43.0.42"}

	changed, err := WriteNodeRegistry(context.Background(), n, info, Config{})
	if err != nil {
		t.Fatalf("WriteNodeRegistry: %v", err)
	}
	if !changed {
		t.Error("expected changed=true")
	}
	if n.readyPolls <= 2 {
		t.Errorf("expected the readiness wait to retry through the not-ready polls, got %d", n.readyPolls)
	}
}

func TestWriteNodeRegistryRestartReadyTimeout(t *testing.T) {
	shrinkRestartTiming(t)
	n := newFakeRegistryNode()
	n.readyAfterRestart = 1 << 30 // /readyz never reports ok
	info := &RegistryInfo{CA: testCAPEM, ClusterIP: "10.43.0.42"}

	_, err := WriteNodeRegistry(context.Background(), n, info, Config{})
	if err == nil || !strings.Contains(err.Error(), "not become ready") {
		t.Fatalf("want a post-restart readiness timeout, got %v", err)
	}
	if !n.restarted {
		t.Error("k3s should have been restarted before the readiness wait")
	}
}

func TestWriteNodeRegistryIncompleteInfo(t *testing.T) {
	for _, info := range []*RegistryInfo{
		nil,
		{ClusterIP: "10.43.0.42"},               // no CA
		{CA: testCAPEM},                         // no ClusterIP
		{CA: testCAPEM, ClusterIP: "not-an-ip"}, // ClusterIP not an IP
	} {
		if _, err := WriteNodeRegistry(context.Background(), newFakeRegistryNode(), info, Config{}); err == nil {
			t.Errorf("expected an error for incomplete info %+v", info)
		}
	}
}

func TestWriteNodeRegistrySudoPrefixes(t *testing.T) {
	shrinkRestartTiming(t)
	n := newFakeRegistryNode()
	info := &RegistryInfo{CA: testCAPEM, ClusterIP: "10.43.0.42"}
	if _, err := WriteNodeRegistry(context.Background(), n, info, Config{Sudo: true}); err != nil {
		t.Fatalf("WriteNodeRegistry: %v", err)
	}
	for _, want := range []string{"sudo tee ", "sudo systemctl restart k3s", "sudo /usr/local/bin/k3s kubectl get --raw=/readyz"} {
		if !hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, want) }) {
			t.Errorf("expected a command containing %q under Sudo", want)
		}
	}
}

func TestRenderHostsReplacesManagedLine(t *testing.T) {
	// A first write appends our marked line; a second write at a new IP replaces
	// it in place, never duplicating and never touching other entries.
	first := renderHosts("127.0.0.1 localhost\n10.0.0.5 other.host\n", "10.43.0.42", registryHost)
	if strings.Count(first, registryHost) != 1 {
		t.Fatalf("expected exactly one managed entry:\n%s", first)
	}
	second := renderHosts(first, "10.43.0.99", registryHost)
	if strings.Count(second, registryHost) != 1 {
		t.Errorf("re-render duplicated the managed entry:\n%s", second)
	}
	if !strings.Contains(second, "10.43.0.99 "+registryHost+registryHostsMarker) {
		t.Errorf("re-render did not update the IP:\n%s", second)
	}
	for _, keep := range []string{"127.0.0.1 localhost", "10.0.0.5 other.host"} {
		if !strings.Contains(second, keep) {
			t.Errorf("re-render dropped an unrelated entry %q:\n%s", keep, second)
		}
	}
}
