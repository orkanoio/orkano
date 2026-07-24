package install

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/orkanoio/orkano/config"
	"github.com/orkanoio/orkano/internal/ssh"
)

// fakeNode is an in-memory stand-in for the server node: it records every
// command, decodes base64 writes into a files map, answers `cat`, and answers
// the readiness `kubectl get … jsonpath` polls from a scriptable state.
type fakeNode struct {
	files       map[string]string
	cmds        []string
	secrets     map[string]string // applied secret name -> rendered manifest
	appliedCRDs []string
	noNS        bool // when true, `get namespace` reports not-found
	crdWaitFail bool
	failSecret  string // secret name whose `kubectl apply` fails

	// runErr maps a substring of the RAW command to a transport error, so the
	// "couldn't run at all" branches (distinct from a non-zero exit, which is a
	// Result) are reachable from tests.
	runErr map[string]error

	// readiness scripting, keyed by "ns/kind/name".
	readyAfter map[string]int  // polls before the workload reports ready
	pollCount  map[string]int  // polls seen so far
	notFound   map[string]bool // never applied (always exits non-zero)
}

func newFakeNode() *fakeNode {
	return &fakeNode{
		files:      map[string]string{},
		secrets:    map[string]string{},
		readyAfter: map[string]int{},
		pollCount:  map[string]int{},
		notFound:   map[string]bool{},
	}
}

func (f *fakeNode) Run(_ context.Context, raw string) (ssh.Result, error) {
	f.cmds = append(f.cmds, raw)
	for sub, err := range f.runErr {
		if strings.Contains(raw, sub) {
			return ssh.Result{}, err
		}
	}
	cmd := strings.ReplaceAll(raw, "sudo ", "") // sudo never appears in a base64 payload

	switch {
	case strings.Contains(cmd, "| base64 -d |") && strings.Contains(cmd, "kubectl apply -f -"):
		name, manifest := parseSecretApply(cmd)
		if name != "" && name == f.failSecret {
			return ssh.Result{Stderr: "apply refused", ExitStatus: 1}, nil
		}
		f.secrets[name] = manifest
		return ssh.Result{}, nil

	case strings.Contains(cmd, "kubectl apply -f "):
		f.appliedCRDs = append(f.appliedCRDs, kubectlApplyPath(cmd))
		return ssh.Result{}, nil

	case strings.Contains(cmd, "kubectl wait --for=condition=Established"):
		if f.crdWaitFail {
			return ssh.Result{Stderr: "timed out waiting for the condition", ExitStatus: 1}, nil
		}
		return ssh.Result{}, nil

	case strings.Contains(cmd, "kubectl get namespace "):
		if f.noNS {
			return ssh.Result{Stderr: "NotFound", ExitStatus: 1}, nil
		}
		return ssh.Result{Stdout: "namespace/orkano-system\n"}, nil

	case strings.Contains(cmd, "kubectl -n") && strings.Contains(cmd, "get secret "):
		name := secretNameArg(cmd)
		if _, ok := f.secrets[name]; ok {
			return ssh.Result{Stdout: "secret/" + name + "\n"}, nil
		}
		return ssh.Result{Stderr: "NotFound", ExitStatus: 1}, nil

	case strings.Contains(cmd, "| base64 -d |"):
		p, c, appendMode := parseWrite(cmd)
		if appendMode {
			f.files[p] += c
		} else {
			f.files[p] = c
		}
		return ssh.Result{}, nil

	case strings.HasPrefix(cmd, "mv "):
		// chunked finalize: `mv PATH.tmp PATH [&& chmod …]`
		fields := strings.Fields(cmd)
		src, dst := fields[1], fields[2]
		// Real mv exits 1 on a missing source and creates nothing; modelling it
		// as success would let a wrong source path pass as an empty file.
		c, ok := f.files[src]
		if !ok {
			return ssh.Result{Stderr: "mv: cannot stat '" + src + "': No such file or directory", ExitStatus: 1}, nil
		}
		f.files[dst] = c
		delete(f.files, src)
		return ssh.Result{}, nil

	case strings.HasPrefix(cmd, "rm -f "):
		delete(f.files, strings.TrimPrefix(cmd, "rm -f "))
		return ssh.Result{}, nil

	case strings.HasPrefix(cmd, "cat "):
		p := strings.TrimPrefix(cmd, "cat ")
		if c, ok := f.files[p]; ok {
			return ssh.Result{Stdout: c}, nil
		}
		return ssh.Result{Stderr: "No such file or directory", ExitStatus: 1}, nil

	case strings.Contains(cmd, "kubectl -n") && strings.Contains(cmd, "readyReplicas"):
		key := readinessKey(cmd)
		if f.notFound[key] {
			return ssh.Result{Stderr: "NotFound", ExitStatus: 1}, nil
		}
		f.pollCount[key]++
		if f.pollCount[key] > f.readyAfter[key] {
			return ssh.Result{Stdout: "1"}, nil
		}
		return ssh.Result{Stdout: ""}, nil // zero ready replicas

	default:
		return ssh.Result{}, nil
	}
}

// parseWrite extracts the destination path, decoded content, and whether the
// write appends, from an ensureFile command of the form
// `…printf %s 'BASE64' | base64 -d | …tee [-a ]PATH >/dev/null…`.
func parseWrite(cmd string) (string, string, bool) {
	const marker = "printf %s '"
	start := strings.Index(cmd, marker) + len(marker)
	end := strings.Index(cmd, "' | base64 -d")
	dec, _ := base64.StdEncoding.DecodeString(cmd[start:end])
	rest := cmd[strings.Index(cmd, "tee ")+len("tee "):]
	appendMode := false
	if strings.HasPrefix(rest, "-a ") {
		appendMode = true
		rest = strings.TrimPrefix(rest, "-a ")
	}
	p := strings.TrimSpace(strings.SplitN(rest, " >/dev/null", 2)[0])
	return p, string(dec), appendMode
}

// parseSecretApply decodes the secret manifest piped into `kubectl apply -f -`
// and returns the Secret's name and the rendered manifest.
func parseSecretApply(cmd string) (string, string) {
	const marker = "printf %s '"
	start := strings.Index(cmd, marker) + len(marker)
	end := strings.Index(cmd, "' | base64 -d")
	dec, _ := base64.StdEncoding.DecodeString(cmd[start:end])
	manifest := string(dec)
	for _, line := range strings.Split(manifest, "\n") {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "name:") {
			return strings.TrimSpace(strings.TrimPrefix(s, "name:")), manifest
		}
	}
	return "", manifest
}

// secretNameArg parses the name from `kubectl -n NS get secret NAME -o name`.
func secretNameArg(cmd string) string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if f == "secret" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func kubectlApplyPath(cmd string) string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if f == "-f" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// readinessKey parses "ns/kind/name" from a `kubectl -n NS get KIND NAME …`.
func readinessKey(cmd string) string {
	fields := strings.Fields(cmd)
	var ns, kind, name string
	for i, f := range fields {
		switch f {
		case "-n":
			ns = fields[i+1]
		case "get":
			kind, name = fields[i+1], fields[i+2]
		}
	}
	return ns + "/" + kind + "/" + name
}

func wrote(cmds []string) bool {
	for _, c := range cmds {
		if strings.Contains(c, "| base64 -d |") {
			return true
		}
	}
	return false
}

func cmdIndex(cmds []string, pred func(string) bool) int {
	for i, c := range cmds {
		if pred(c) {
			return i
		}
	}
	return -1
}

func TestApplyWritesAllStaticManifests(t *testing.T) {
	n := newFakeNode()
	res, err := Apply(context.Background(), n, Config{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected Changed=true on a fresh apply")
	}

	files, err := staticManifests()
	if err != nil {
		t.Fatalf("staticManifests: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no embedded manifests")
	}
	base := path.Join(DefaultAutoDeployDir, manifestSubdir)
	for _, f := range files {
		dest := path.Join(base, f.name)
		got, ok := n.files[dest]
		if !ok {
			t.Errorf("manifest %s not written to %s", f.name, dest)
			continue
		}
		if got != string(f.content) {
			t.Errorf("manifest %s written with wrong content", f.name)
		}
	}
	// Sanity: a few known manifests landed under their flattened names (CRDs,
	// namespaces, the platform DB, the registry).
	for _, want := range []string{
		"crd-orkano-io-apps.yaml",
		"namespaces-namespaces.yaml",
		"components-platform-postgres.yaml",
		"registry-registry.yaml",
		"cert-manager-cert-manager.yaml",
		"traefik-traefik-redirect.yaml",
	} {
		if _, ok := n.files[path.Join(base, want)]; !ok {
			t.Errorf("expected %s to be deployed", want)
		}
	}
	// Writes carry the root-only mode.
	if !hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, "chmod 0600 ") }) {
		t.Error("expected writes to chmod 0600")
	}
}

func TestApplyAppliesCRDsBeforeComponents(t *testing.T) {
	n := newFakeNode()
	if _, err := Apply(context.Background(), n, Config{Version: "2.0.0"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	base := path.Join(DefaultAutoDeployDir, manifestSubdir)
	for _, name := range []string{
		"crd-orkano-io-apps.yaml",
		"crd-orkano-io-builds.yaml",
		"crd-orkano-io-domains.yaml",
		"crd-orkano-io-mongoes.yaml",
		"crd-orkano-io-postgreses.yaml",
	} {
		want := path.Join(base, name)
		found := false
		for _, got := range n.appliedCRDs {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("CRD manifest %s was not explicitly applied; got %v", want, n.appliedCRDs)
		}
	}

	waitIdx := cmdIndex(n.cmds, func(c string) bool {
		return strings.Contains(c, "kubectl wait --for=condition=Established") &&
			strings.Contains(c, "crd/apps.orkano.io") &&
			strings.Contains(c, "crd/mongoes.orkano.io") &&
			strings.Contains(c, "crd/postgreses.orkano.io")
	})
	operatorWriteIdx := cmdIndex(n.cmds, func(c string) bool {
		return strings.Contains(c, "components-operator-deployment.yaml") && strings.Contains(c, "| base64 -d |")
	})
	if waitIdx < 0 {
		t.Fatal("expected a CRD Established wait")
	}
	if operatorWriteIdx < 0 {
		t.Fatal("expected the operator manifest to be written")
	}
	if waitIdx > operatorWriteIdx {
		t.Fatalf("operator manifest was written before CRDs were established (wait cmd %d, operator write %d)", waitIdx, operatorWriteIdx)
	}
}

func TestApplyFailsWhenCRDsDoNotEstablish(t *testing.T) {
	n := newFakeNode()
	n.crdWaitFail = true
	_, err := Apply(context.Background(), n, Config{Version: "2.0.0"})
	if err == nil {
		t.Fatal("expected Apply to fail when CRDs do not establish")
	}
	if !strings.Contains(err.Error(), "wait for CRDs to be established") {
		t.Fatalf("expected CRD wait error, got %v", err)
	}
	if _, ok := n.files[path.Join(DefaultAutoDeployDir, manifestSubdir, "components-operator-deployment.yaml")]; ok {
		t.Fatal("operator manifest should not be written before CRDs are established")
	}
}

func TestApplySecretsVaultOptIn(t *testing.T) {
	base := path.Join(DefaultAutoDeployDir, manifestSubdir)
	esoPath := path.Join(base, "external-secrets-external-secrets.yaml")

	// Off by default: the vendored ESO set must never join the base write set
	// (ADR-0018 decision 2).
	n := newFakeNode()
	if _, err := Apply(context.Background(), n, Config{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := n.files[esoPath]; ok {
		t.Fatal("external-secrets manifest written without --secrets-vault")
	}

	// Opted in: written byte-identically (the ~1MB file rides the chunked
	// write path).
	n = newFakeNode()
	res, err := Apply(context.Background(), n, Config{SecretsVault: true})
	if err != nil {
		t.Fatalf("Apply with SecretsVault: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected Changed=true")
	}
	want, err := config.ExternalSecretsManifest.ReadFile("external-secrets/external-secrets.yaml")
	if err != nil {
		t.Fatalf("read embedded external-secrets: %v", err)
	}
	got, ok := n.files[esoPath]
	if !ok {
		t.Fatalf("external-secrets manifest not written to %s", esoPath)
	}
	if got != string(want) {
		t.Fatal("external-secrets manifest written with wrong content")
	}
}

func TestSecretsVaultReadinessTargets(t *testing.T) {
	targets := SecretsVaultReadinessTargets()
	if err := validateTargets(targets); err != nil {
		t.Fatalf("SecretsVaultReadinessTargets must validate: %v", err)
	}
	want := map[string]bool{
		"external-secrets":                 false,
		"external-secrets-webhook":         false,
		"external-secrets-cert-controller": false,
	}
	for _, w := range targets {
		if w.Namespace != "external-secrets" || w.Kind != "deployment" {
			t.Errorf("unexpected target %+v", w)
		}
		want[w.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing readiness target %s", name)
		}
	}
}

func TestDefaultReadinessTargetsIncludesDashboard(t *testing.T) {
	want := Workload{Namespace: "orkano-system", Kind: "deployment", Name: "orkano-dashboard"}
	for _, w := range DefaultReadinessTargets() {
		if w == want {
			return
		}
	}
	t.Errorf("DefaultReadinessTargets must wait for the dashboard Deployment %+v", want)
}

func TestApplyIdempotent(t *testing.T) {
	n := newFakeNode()
	files, err := staticManifests()
	if err != nil {
		t.Fatalf("staticManifests: %v", err)
	}
	// Pre-seed the node with the exact contents Apply would write.
	base := path.Join(DefaultAutoDeployDir, manifestSubdir)
	for _, f := range files {
		n.files[path.Join(base, f.name)] = string(f.content)
	}

	res, err := Apply(context.Background(), n, Config{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Changed {
		t.Error("expected Changed=false when nothing differs")
	}
	if wrote(n.cmds) {
		t.Error("expected no write commands on an idempotent re-run")
	}
	// The CRD apply + Established wait deliberately still run: the gate is about
	// cluster state, which unchanged files cannot prove (a crashed earlier run or
	// a still-converging server). Both are idempotent no-ops on a healthy node.
	if len(n.appliedCRDs) == 0 {
		t.Error("expected the CRD apply to converge even on a no-op re-run")
	}
}

func TestApplyMigratesCollidingCRDManifestNames(t *testing.T) {
	n := newFakeNode()
	files, err := staticManifests()
	if err != nil {
		t.Fatalf("staticManifests: %v", err)
	}
	base := path.Join(DefaultAutoDeployDir, manifestSubdir)
	var migrated int
	replacementAlreadyPresent := false
	for _, f := range files {
		if f.legacyName == "" {
			n.files[path.Join(base, f.name)] = string(f.content)
			continue
		}
		n.files[path.Join(base, f.legacyName)] = string(f.content)
		if !replacementAlreadyPresent {
			n.files[path.Join(base, f.name)] = string(f.content)
			replacementAlreadyPresent = true
		}
		migrated++
	}
	if migrated != 5 {
		t.Fatalf("expected the five Orkano CRDs to need migration, got %d", migrated)
	}

	res, err := Apply(context.Background(), n, Config{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Changed {
		t.Fatal("expected the legacy-name migration to report a change")
	}
	for _, f := range files {
		if f.legacyName == "" {
			continue
		}
		if _, ok := n.files[path.Join(base, f.legacyName)]; ok {
			t.Errorf("legacy manifest %s remains", f.legacyName)
		}
		if got := n.files[path.Join(base, f.name)]; got != string(f.content) {
			t.Errorf("replacement manifest %s has wrong content", f.name)
		}
	}
	if !hasCmd(n.cmds, func(cmd string) bool { return strings.HasPrefix(cmd, "rm -f ") }) {
		t.Fatal("expected a partially migrated install to remove the legacy duplicate")
	}

	res, err = Apply(context.Background(), n, Config{})
	if err != nil {
		t.Fatalf("idempotent Apply: %v", err)
	}
	if res.Changed {
		t.Fatal("expected migrated manifests to be idempotent on the next run")
	}
}

// TestMigrateLegacyManifests drives the migration directly, where the
// intermediate state is still observable. Through Apply the following
// ensureFile overwrites every file with the correct bytes, so an mv that moved
// the wrong file — or never ran — is indistinguishable from a correct one.
func TestMigrateLegacyManifests(t *testing.T) {
	base := path.Join(DefaultAutoDeployDir, manifestSubdir)
	files := []manifestFile{
		{name: "crd-a.yaml", legacyName: "crd-a.io_x.yaml", content: []byte("new-a")},
		{name: "crd-b.yaml", legacyName: "crd-b.io_x.yaml", content: []byte("new-b")},
		{name: "plain.yaml", content: []byte("plain")},
	}

	for _, sudo := range []bool{false, true} {
		t.Run(fmt.Sprintf("sudo=%v", sudo), func(t *testing.T) {
			n := newFakeNode()
			// crd-a has no replacement yet -> mv; crd-b already has one -> rm.
			n.files[path.Join(base, "crd-a.io_x.yaml")] = "legacy-a"
			n.files[path.Join(base, "crd-b.io_x.yaml")] = "legacy-b"
			n.files[path.Join(base, "crd-b.yaml")] = "stale-b"
			n.files[path.Join(base, "plain.yaml")] = "plain"

			changed, err := newNode(n, sudo, nil).migrateLegacyManifests(context.Background(), base, files)
			if err != nil {
				t.Fatalf("migrateLegacyManifests: %v", err)
			}
			if !changed {
				t.Fatal("expected the migration to report a change")
			}

			// mv must land the legacy bytes at the RIGHT destination.
			if got := n.files[path.Join(base, "crd-a.yaml")]; got != "legacy-a" {
				t.Errorf("crd-a.yaml = %q, want the migrated legacy content", got)
			}
			// rm must leave an existing replacement untouched.
			if got := n.files[path.Join(base, "crd-b.yaml")]; got != "stale-b" {
				t.Errorf("crd-b.yaml = %q, want the existing replacement preserved", got)
			}
			for _, legacy := range []string{"crd-a.io_x.yaml", "crd-b.io_x.yaml"} {
				if _, ok := n.files[path.Join(base, legacy)]; ok {
					t.Errorf("legacy manifest %s remains", legacy)
				}
			}
			if got := n.files[path.Join(base, "plain.yaml")]; got != "plain" {
				t.Errorf("a file with no legacy name must not be touched, got %q", got)
			}

			// The raw commands carry the sudo prefix: /var/lib/rancher is
			// root-owned, so a non-root --ssh-user needs it or every upgrade
			// fails at the rename.
			wantMv := "mv " + path.Join(base, "crd-a.io_x.yaml") + " " + path.Join(base, "crd-a.yaml")
			wantRm := "rm -f " + path.Join(base, "crd-b.io_x.yaml")
			if sudo {
				wantMv, wantRm = "sudo "+wantMv, "sudo "+wantRm
			}
			for _, want := range []string{wantMv, wantRm} {
				if !hasCmd(n.cmds, func(c string) bool { return c == want }) {
					t.Errorf("expected the exact command %q, got %v", want, n.cmds)
				}
			}
		})
	}
}

// TestMigrateLegacyManifestsSurfacesTransportErrors pins that a connection blip
// is never mistaken for "no legacy file present". Swallowing it would leave the
// colliding v0.1.0 filenames in place while Apply reports success — the exact
// bug this code exists to fix, made silent.
func TestMigrateLegacyManifestsSurfacesTransportErrors(t *testing.T) {
	base := path.Join(DefaultAutoDeployDir, manifestSubdir)
	files := []manifestFile{{name: "crd-a.yaml", legacyName: "crd-a.io_x.yaml", content: []byte("new-a")}}
	boom := errors.New("connection reset")

	tests := map[string]struct {
		errOn string
		seed  map[string]string
	}{
		"legacy read fails":      {errOn: "cat " + path.Join(base, "crd-a.io_x.yaml")},
		"replacement read fails": {errOn: "cat " + path.Join(base, "crd-a.yaml"), seed: map[string]string{path.Join(base, "crd-a.io_x.yaml"): "legacy-a"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			n := newFakeNode()
			for p, c := range tc.seed {
				n.files[p] = c
			}
			n.runErr = map[string]error{tc.errOn: boom}
			if _, err := newNode(n, false, nil).migrateLegacyManifests(context.Background(), base, files); !errors.Is(err, boom) {
				t.Fatalf("expected the transport error to surface, got %v", err)
			}
		})
	}

	t.Run("rename fails", func(t *testing.T) {
		n := newFakeNode()
		n.files[path.Join(base, "crd-a.io_x.yaml")] = "legacy-a"
		n.runErr = map[string]error{"mv ": boom}
		if _, err := newNode(n, false, nil).migrateLegacyManifests(context.Background(), base, files); !errors.Is(err, boom) {
			t.Fatalf("expected the rename failure to surface, got %v", err)
		}
	})
}

// TestApplyMigrationAbortsBeforeCRDApply pins that a failed migration stops the
// install rather than proceeding onto a half-renamed manifest directory.
func TestApplyMigrationAbortsBeforeCRDApply(t *testing.T) {
	n := newFakeNode()
	base := path.Join(DefaultAutoDeployDir, manifestSubdir)
	files, err := staticManifests()
	if err != nil {
		t.Fatalf("staticManifests: %v", err)
	}
	for _, f := range files {
		if f.legacyName != "" {
			n.files[path.Join(base, f.legacyName)] = string(f.content)
		}
	}
	boom := errors.New("connection reset")
	n.runErr = map[string]error{"mv ": boom}

	if _, err := Apply(context.Background(), n, Config{}); !errors.Is(err, boom) {
		t.Fatalf("expected Apply to surface the migration failure, got %v", err)
	}
	if len(n.appliedCRDs) != 0 {
		t.Errorf("no CRD should be applied after a failed migration, got %v", n.appliedCRDs)
	}
}

// TestApplyNeverWritesAManifestPathInPlace pins the atomicity of every write.
// k3s re-reads a manifest on any mtime change, so a `tee` straight onto the
// live path exposes a truncate-then-write window in which k3s can read an empty
// document and prune everything that AddOn created.
func TestApplyNeverWritesAManifestPathInPlace(t *testing.T) {
	n := newFakeNode()
	if _, err := Apply(context.Background(), n, Config{Version: "2.0.0", ReceiverHost: "hooks.example.com"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, raw := range n.cmds {
		cmd := strings.ReplaceAll(raw, "sudo ", "")
		if !strings.Contains(cmd, "| base64 -d |") || strings.Contains(cmd, "kubectl apply -f -") {
			continue
		}
		target, _, _ := parseWrite(cmd)
		if !strings.HasSuffix(target, ".tmp") {
			t.Errorf("write targets %s directly; every write must land on a .tmp and be renamed", target)
		}
	}
}

func TestManifestFilename(t *testing.T) {
	ok := map[string]string{
		"crd/orkano.io_apps.yaml":        "crd-orkano-io-apps.yaml",
		"components/dashboard.yaml":      "components-dashboard.yaml",
		"netpol/orkano-builds.yaml":      "netpol-orkano-builds.yaml",
		"cert-manager/cert-manager.yaml": "cert-manager-cert-manager.yaml",
		"registry/internal-ca.yaml":      "registry-internal-ca.yaml",
		"external-secrets/external.yaml": "external-secrets-external.yaml",
		"a/b/c.yaml":                     "a-b-c.yaml",
		"crd/orkano.io___weird__.yaml":   "crd-orkano-io-weird.yaml",
	}
	for in, want := range ok {
		got, err := manifestFilename(in)
		if err != nil {
			t.Errorf("manifestFilename(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("manifestFilename(%q) = %q, want %q", in, got, want)
		}
	}

	// An uppercase letter must be refused, not silently dropped: dropping it
	// yields a wrong-but-valid AddOn name (crd/orkano.io_Apps.yaml would become
	// "crd-orkano-io-pps") that nothing downstream can catch.
	bad := []string{
		"crd/orkano.io_Apps.yaml",
		"registry/RÉGISTRY.yaml",
		"netpol/orkano-builds.yml",
		"netpol/orkano-builds",
		"___.yaml",
	}
	for _, in := range bad {
		if got, err := manifestFilename(in); err == nil {
			t.Errorf("manifestFilename(%q) = %q, want an error", in, got)
		}
	}
}

// TestManifestFilesHaveUniqueK3sSafeAddonNames is the drift guard: every name
// Apply can write must be k3s-safe and unique. It runs over each gating knob,
// because a conditionally-rendered file (the receiver Ingress) would otherwise
// never reach the validator, and component names bypass manifestFilename's
// folding — which is exactly where an unsafe name gets introduced by hand.
func TestManifestFilesHaveUniqueK3sSafeAddonNames(t *testing.T) {
	configs := map[string]Config{
		"defaults":      {Version: "test"},
		"receiver host": {Version: "test", ReceiverHost: "hooks.example.com"},
		"secrets vault": {Version: "test", SecretsVault: true},
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			files, err := writeSet(cfg)
			if err != nil {
				t.Fatalf("writeSet: %v", err)
			}
			if err := validateManifestFiles(files); err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				// k3s names an AddOn after the basename up to the FIRST dot, so
				// any dot before .yaml silently truncates the identity.
				if strings.Count(f.name, ".") != 1 {
					t.Errorf("manifest %s has punctuation k3s folds into a colliding AddOn name", f.name)
				}
				// A single unprefixed segment claims a generic global AddOn name
				// ("dashboard", "receiver") that any operator-dropped file under
				// the manifests tree would fight over.
				if !strings.Contains(strings.TrimSuffix(f.name, ".yaml"), "-") {
					t.Errorf("manifest %s has an unprefixed AddOn name in a globally shared space", f.name)
				}
			}
		})
	}

	// The receiver Ingress is the conditional file the guard exists to reach.
	files, err := writeSet(Config{Version: "test", ReceiverHost: "hooks.example.com"})
	if err != nil {
		t.Fatalf("writeSet: %v", err)
	}
	found := false
	for _, f := range files {
		if f.name == "components-receiver-ingress.yaml" {
			found = true
		}
	}
	if !found {
		t.Fatal("the conditional receiver Ingress never reached the uniqueness guard")
	}
}

func TestValidateManifestFilesRejectsK3sAddonCollisions(t *testing.T) {
	tests := map[string][]manifestFile{
		"punctuation before extension": {
			{name: "crd-orkano.io_apps.yaml"},
		},
		"duplicate": {
			{name: "crd-orkano-io-apps.yaml"},
			{name: "crd-orkano-io-apps.yaml"},
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateManifestFiles(files); err == nil {
				t.Fatal("expected invalid manifest names to be rejected")
			}
		})
	}
}

func TestApplyWaitsForReadiness(t *testing.T) {
	defer swapPollInterval(time.Millisecond)()

	n := newFakeNode()
	targets := []Workload{
		{Namespace: "orkano-system", Kind: "statefulset", Name: "orkano-postgres"},
		{Namespace: "orkano-system", Kind: "deployment", Name: "orkano-registry"},
	}
	n.readyAfter["orkano-system/statefulset/orkano-postgres"] = 2
	n.readyAfter["orkano-system/deployment/orkano-registry"] = 1

	res, err := Apply(context.Background(), n, Config{ReadinessTargets: targets, WaitTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Changed {
		t.Error("expected the manifests to be written")
	}
}

func TestApplyReadinessTimeout(t *testing.T) {
	defer swapPollInterval(time.Millisecond)()

	n := newFakeNode()
	target := Workload{Namespace: "orkano-system", Kind: "deployment", Name: "orkano-operator"}
	n.notFound["orkano-system/deployment/orkano-operator"] = true

	_, err := Apply(context.Background(), n, Config{
		ReadinessTargets: []Workload{target},
		WaitTimeout:      30 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "orkano-operator") {
		t.Errorf("timeout error should name the pending workload, got: %v", err)
	}
}

func TestApplyReadinessTimeoutReturnsGeneratedBootstrapToken(t *testing.T) {
	defer swapPollInterval(time.Millisecond)()

	n := newFakeNode()
	target := Workload{Namespace: "orkano-system", Kind: "deployment", Name: "orkano-operator"}
	n.notFound["orkano-system/deployment/orkano-operator"] = true

	res, err := Apply(context.Background(), n, Config{
		Version:          "2.0.0",
		ReadinessTargets: []Workload{target},
		WaitTimeout:      30 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if res == nil || res.BootstrapToken == "" {
		t.Fatalf("expected partial result with bootstrap token, got %#v", res)
	}
	if !res.Changed {
		t.Error("a fresh install that wrote manifests and secrets must report Changed even on a partial result")
	}
	if !strings.Contains(err.Error(), "orkano-operator") {
		t.Errorf("timeout error should name the pending workload, got: %v", err)
	}
}

func TestApplySecretFailureAfterTokenStillReturnsToken(t *testing.T) {
	// The bootstrap-token secret is created before the two M2.6 placeholders
	// (orkano-github-app, orkano-oidc); a failure on one of THOSE must still
	// surface the freshly-created plaintext, or the admin is locked out exactly
	// as in the original live-v0.0.2 incident.
	n := newFakeNode()
	n.failSecret = "orkano-oidc"
	res, err := Apply(context.Background(), n, Config{Version: "2.0.0"})
	if err == nil {
		t.Fatal("expected the failed secret apply to error")
	}
	if res == nil || res.BootstrapToken == "" {
		t.Fatalf("expected partial result carrying the bootstrap token, got %#v", res)
	}
	if !res.Changed {
		t.Error("secrets were created before the failure; Changed must be true on the partial result")
	}
}

func TestApplySecretFailureBeforeTokenReturnsNoToken(t *testing.T) {
	// A failure before the bootstrap-token secret exists must NOT invent a
	// token: nothing was created, so there is nothing to print or lose.
	n := newFakeNode()
	n.failSecret = "orkano-postgres-superuser"
	res, err := Apply(context.Background(), n, Config{Version: "2.0.0"})
	if err == nil {
		t.Fatal("expected the failed secret apply to error")
	}
	if res != nil && res.BootstrapToken != "" {
		t.Fatalf("no bootstrap-token secret was created; token must be empty, got %q", res.BootstrapToken)
	}
}

func TestApplySudoPrefixes(t *testing.T) {
	defer swapPollInterval(time.Millisecond)()

	n := newFakeNode()
	target := Workload{Namespace: "orkano-system", Kind: "deployment", Name: "orkano-registry"}
	if _, err := Apply(context.Background(), n, Config{
		Sudo:             true,
		ReadinessTargets: []Workload{target},
		WaitTimeout:      time.Second,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, want := range []string{"sudo cat ", "sudo tee ", "sudo /usr/local/bin/k3s kubectl"} {
		if !hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, want) }) {
			t.Errorf("expected a command containing %q under Sudo", want)
		}
	}
}

func TestApplyRejectsInvalidTarget(t *testing.T) {
	for _, w := range []Workload{
		{Namespace: "orkano-system", Kind: "pod", Name: "x"},                  // unsupported kind
		{Namespace: "orkano-system", Kind: "deployment", Name: "x; rm -rf /"}, // injection attempt
		{Namespace: "bad ns", Kind: "deployment", Name: "x"},                  // space in namespace
	} {
		if _, err := Apply(context.Background(), newFakeNode(), Config{ReadinessTargets: []Workload{w}}); err == nil {
			t.Errorf("expected rejection for target %+v", w)
		}
	}
}

func TestApplyNilRunner(t *testing.T) {
	if _, err := Apply(context.Background(), nil, Config{}); err == nil {
		t.Fatal("expected an error for a nil runner")
	}
}

func TestEnsureFileChunkedRoundTrip(t *testing.T) {
	n := newFakeNode()
	nd := newNode(n, false, nil)

	// Content whose base64 exceeds maxInlineBase64, forcing the chunked path.
	content := bytes.Repeat([]byte("orkano-cert-manager-payload\n"), 8000) // ~216 KiB
	if len(content) <= maxInlineBase64 {
		t.Fatal("test content is not large enough to chunk")
	}

	changed, err := nd.ensureFile(context.Background(), "/var/lib/rancher/k3s/server/manifests/orkano/big.yaml", content, "0600")
	if err != nil {
		t.Fatalf("ensureFile: %v", err)
	}
	if !changed {
		t.Fatal("expected a write")
	}
	if got := n.files["/var/lib/rancher/k3s/server/manifests/orkano/big.yaml"]; got != string(content) {
		t.Fatalf("chunked write did not round-trip: got %d bytes, want %d", len(got), len(content))
	}
	// The chunked path uses append (tee -a) and an atomic rename.
	if !hasCmd(n.cmds, func(c string) bool { return strings.Contains(c, "tee -a ") }) {
		t.Error("expected appended chunks (tee -a) for a large file")
	}
	if !hasCmd(n.cmds, func(c string) bool { return strings.HasPrefix(c, "mv ") }) {
		t.Error("expected an atomic rename (mv) to finalize the chunked write")
	}
	// No single inline command should carry the whole oversize payload.
	for _, c := range n.cmds {
		if len(c) > maxInlineBase64+4096 {
			t.Errorf("a command exceeded the inline bound (%d chars) — would risk E2BIG", len(c))
		}
	}
}

// hasCmd reports whether any recorded command satisfies pred.
func hasCmd(cmds []string, pred func(string) bool) bool {
	for _, c := range cmds {
		if pred(c) {
			return true
		}
	}
	return false
}

// swapPollInterval temporarily shrinks the readiness poll cadence and returns a
// restore function.
func swapPollInterval(d time.Duration) func() {
	prev := waitPollInterval
	waitPollInterval = d
	return func() { waitPollInterval = prev }
}
