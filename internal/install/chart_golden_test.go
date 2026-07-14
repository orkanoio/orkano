package install

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The golden-render half of the ADR-0019 fork-b drift guard: for equivalent
// values, `helm template` over charts/orkano must render byte-identical
// component documents to renderComponents — the two install paths deploy ONE
// manifest set. The static half is chart_test.go's byte-equality; this covers
// the templated half (templates/components/ mirrors
// internal/install/templates/*.yaml.tmpl).
//
// Runs ONLY under ORKANO_HELM_BIN (`make verify-chart` sets it to the
// sha256-pinned helm 4; CI runs that target as its own `chart` job) and skips
// otherwise — deliberately NO PATH fallback: helm majors render different
// bytes (the vendor-external-secrets.sh precedent), and a plain `make test`
// must not change behavior with whatever helm the machine happens to carry.
// A wrong-major ORKANO_HELM_BIN FAILS instead of skipping, so the pinned CI
// path can never silently skip.

const chartComponentSource = "# Source: orkano/templates/components/"

// NB: a literal `---` line inside a block scalar would mis-split — none of
// the component templates use block scalars; revisit if one ever does.
var docSeparatorRe = regexp.MustCompile(`(?m)^---\s*$`)

func TestChartComponentGoldenRender(t *testing.T) {
	helm := helmForGolden(t)

	cases := []struct {
		name string
		cfg  Config
		args []string
	}{
		{
			// Explicit tag, everything else at chart defaults: staging ACME, no
			// email, empty allowlist, no receiver host (the Ingress and the
			// dashboard webhook env must be absent on BOTH sides).
			name: "defaults",
			cfg:  Config{Version: "1.2.3"},
			args: []string{"--set", "images.tag=1.2.3"},
		},
		{
			// images.tag left empty so the chart falls back to appVersion — the
			// Go side renders the same version read from Chart.yaml. Exercises
			// every conditional: email line, prod ACME server, joined allowlist,
			// receiver Ingress + the dashboard's ORKANO_WEBHOOK_URL.
			name: "full",
			cfg: Config{
				Version:       chartAppVersion(t),
				ACMEEmail:     "ops@example.com",
				ACMEProd:      true,
				RepoAllowlist: []string{"orkanoio/orkano", "acme/widgets"},
				ReceiverHost:  "hooks.example.com",
			},
			args: []string{
				"--set", "acme.email=ops@example.com",
				"--set", "acme.production=true",
				"--set", "repoAllowlist={orkanoio/orkano,acme/widgets}",
				"--set", "receiver.host=hooks.example.com",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"template", "orkano", chartRoot}, tc.args...)
			cmd := exec.CommandContext(t.Context(), helm, args...)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("helm template: %v\nstderr: %s", err, stderr.String())
			}
			chartDocs := componentDocs(string(out))

			files, err := renderComponents(tc.cfg)
			if err != nil {
				t.Fatalf("renderComponents: %v", err)
			}
			var goDocs []string
			for _, f := range files {
				goDocs = append(goDocs, splitDocs(string(f.content))...)
			}

			assertSameDocSet(t, chartDocs, goDocs)
		})
	}
}

// TestValuesSchemaMirrorsGoValidation pins values.schema.json's patterns to
// components.go's injection-guard regexes — the schema is the Helm-side
// equivalent of renderComponents' input validation, and a drift between them
// would let `helm install` accept a value `orkano init` refuses (or vice
// versa). `^$|` marks values the Go side treats as optional (empty = off).
// Needs no helm, so it runs under plain `make test`.
func TestValuesSchemaMirrorsGoValidation(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartRoot, "values.schema.json"))
	if err != nil {
		t.Fatalf("read values.schema.json: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse values.schema.json: %v", err)
	}
	optional := func(re *regexp.Regexp) string { return "^$|" + re.String() }
	for _, tc := range []struct {
		path []string
		want string
	}{
		{[]string{"images", "tag"}, optional(versionRe)},
		{[]string{"acme", "email"}, optional(emailRe)},
		{[]string{"receiver", "host"}, optional(hostRe)},
		{[]string{"repoAllowlist"}, repoNameRe.String()},
	} {
		if got := schemaPattern(t, schema, tc.path...); got != tc.want {
			t.Errorf("values.schema.json %s pattern %q != components.go regex %q — keep the two validators in sync",
				strings.Join(tc.path, "."), got, tc.want)
		}
	}
}

// schemaPattern walks properties.<seg>… to a leaf's pattern; an array leaf
// descends into items.
func schemaPattern(t *testing.T, schema map[string]any, path ...string) string {
	t.Helper()
	node := schema
	for _, seg := range path {
		props, ok := node["properties"].(map[string]any)
		if !ok {
			t.Fatalf("schema node missing properties on the way to %v", path)
		}
		node, ok = props[seg].(map[string]any)
		if !ok {
			t.Fatalf("schema missing %q on the way to %v", seg, path)
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		node = items
	}
	s, ok := node["pattern"].(string)
	if !ok {
		t.Fatalf("no pattern at %v", path)
	}
	return s
}

// helmForGolden resolves the helm binary and enforces the pinned major.
func helmForGolden(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("ORKANO_HELM_BIN")
	if bin == "" {
		t.Skip("ORKANO_HELM_BIN not set — run `make verify-chart` (sha256-pinned helm; a PATH helm is deliberately not used)")
	}
	out, err := exec.CommandContext(t.Context(), bin, "version", "--template", "{{.Version}}").Output()
	if err != nil {
		t.Fatalf("helm version: %v", err)
	}
	if v := strings.TrimSpace(string(out)); !strings.HasPrefix(v, "v4.") {
		t.Fatalf("ORKANO_HELM_BIN is helm %s; the golden render pins helm 4 (majors render different bytes)", v)
	}
	return bin
}

// chartAppVersion reads appVersion out of Chart.yaml so the tag-defaulting
// case tracks chart bumps instead of breaking on them.
func chartAppVersion(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartRoot, "Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "appVersion:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	t.Fatal("no appVersion in Chart.yaml")
	return ""
}

// componentDocs extracts the documents helm rendered from templates/components/
// (identified by the # Source: header helm prepends) and normalizes them like
// splitDocs. Docs from static.yaml, cert-manager, and external-secrets are
// deliberately ignored — chart_test.go guards those byte-for-byte.
func componentDocs(out string) []string {
	var docs []string
	for _, doc := range docSeparatorRe.Split(out, -1) {
		if !strings.Contains(doc, chartComponentSource) {
			continue
		}
		var keep []string
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(line, "# Source: ") {
				continue
			}
			keep = append(keep, line)
		}
		if d := strings.TrimSpace(strings.Join(keep, "\n")); d != "" {
			docs = append(docs, d)
		}
	}
	return docs
}

// splitDocs splits multi-document YAML on top-level separators and trims each
// document; empty documents are dropped.
func splitDocs(raw string) []string {
	var docs []string
	for _, doc := range docSeparatorRe.Split(raw, -1) {
		if d := strings.TrimSpace(doc); d != "" {
			docs = append(docs, d)
		}
	}
	return docs
}

// assertSameDocSet compares the two renders as document multisets — helm
// reorders documents (kind install order), so document identity, not sequence,
// is the contract. On a mismatch it names the document and, when a same-named
// counterpart exists on the other side, the first differing line.
func assertSameDocSet(t *testing.T, chartDocs, goDocs []string) {
	t.Helper()
	count := make(map[string]int)
	for _, d := range goDocs {
		count[d]++
	}
	for _, d := range chartDocs {
		count[d]--
	}
	if len(chartDocs) != len(goDocs) {
		t.Errorf("chart rendered %d component docs, renderComponents %d", len(chartDocs), len(goDocs))
	}
	reported := make(map[string]bool)
	for d, n := range count {
		if n == 0 || reported[docIdentity(d)] {
			continue
		}
		reported[docIdentity(d)] = true
		side, other := "renderComponents", chartDocs
		if n < 0 {
			side, other = "chart", goDocs
		}
		if peer, ok := findDocByIdentity(other, d); ok {
			line, a, b := firstDiffLine(d, peer)
			t.Errorf("doc %s differs between chart and renderComponents at line %d:\n  %s: %q\n  other: %q",
				docIdentity(d), line, side, a, b)
			continue
		}
		t.Errorf("doc %s rendered only by %s:\n%s", docIdentity(d), side, head(d, 8))
	}
}

// docIdentity is the kind + name header of a manifest, for readable failures.
func docIdentity(doc string) string {
	var kind, name string
	for _, line := range strings.Split(doc, "\n") {
		if v, ok := strings.CutPrefix(line, "kind:"); ok && kind == "" {
			kind = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "name:"); ok && name == "" {
			name = strings.TrimSpace(v)
		}
		if kind != "" && name != "" {
			break
		}
	}
	return kind + "/" + name
}

func findDocByIdentity(docs []string, want string) (string, bool) {
	id := docIdentity(want)
	for _, d := range docs {
		if docIdentity(d) == id {
			return d, true
		}
	}
	return "", false
}

func firstDiffLine(a, b string) (int, string, string) {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return i + 1, al[i], bl[i]
		}
	}
	if len(al) < len(bl) {
		return len(al) + 1, "<end>", bl[len(al)]
	}
	return len(bl) + 1, al[len(bl)], "<end>"
}

func head(doc string, n int) string {
	lines := strings.Split(doc, "\n")
	if len(lines) > n {
		rest := len(lines) - n
		lines = append(lines[:n:n], fmt.Sprintf("… (%d more lines)", rest))
	}
	return strings.Join(lines, "\n")
}
