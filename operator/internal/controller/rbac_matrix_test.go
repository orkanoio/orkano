package controller

// docs/security/rbac-matrix.md promises that any diff between the matrix and
// the literal manifests in config/ is a bug. These tests are that promise:
// the doc's tables are parsed into permission tuples, the manifests are
// expanded into the same tuple space, and the two sets must match exactly —
// then every tuple is walked through the live RBAC authorizer (envtest runs
// the real kube-apiserver with --authorization-mode=RBAC) via
// SubjectAccessReview, allowed AND denied. A doc row the parser cannot
// understand fails loudly rather than silently dropping coverage.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	serializerjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	buildsNamespace = "orkano-builds"

	dashboardIdentity = "system:serviceaccount:orkano-system:orkano-dashboard"
	operatorIdentity  = "system:serviceaccount:orkano-system:orkano-operator"
	receiverIdentity  = "system:serviceaccount:orkano-system:orkano-receiver"
	buildIdentity     = "system:serviceaccount:orkano-builds:orkano-build"

	humanIdentityPrefix = "role:"
)

// rbacTuple is one cell of the matrix: identity may do verb on group/resource
// in namespace. namespace "" means cluster-scoped. Human roles use the
// identity "role:<name>" because they ship unbound — bindings to real OIDC
// identities are created at install time, not in config/.
type rbacTuple struct {
	identity  string
	namespace string
	group     string
	resource  string
	verb      string
}

func (tu rbacTuple) String() string {
	ns := tu.namespace
	if ns == "" {
		ns = "<cluster>"
	}
	group := tu.group
	if group == "" {
		group = "core"
	}
	return fmt.Sprintf("%s: %s %s/%s in %s", tu.identity, tu.verb, group, tu.resource, ns)
}

var rbacKnownVerbs = map[string]bool{
	"get": true, "list": true, "watch": true, "create": true,
	"update": true, "patch": true, "delete": true, "impersonate": true,
}

var orkanoNamespaces = map[string]bool{
	systemNamespace: true, appsNamespace: true, buildsNamespace: true,
}

// --- rbac-matrix.md parsing ---

var (
	resourceClauseRe = regexp.MustCompile(`^(.+)\(([^)]+)\)$`)
	codeSpanRe       = regexp.MustCompile("`([^`]+)`")
	separatorCellRe  = regexp.MustCompile(`^-+$`)
)

func parseRBACMatrixDoc(t *testing.T) map[rbacTuple]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "security", "rbac-matrix.md"))
	if err != nil {
		t.Fatalf("failed to read rbac-matrix.md: %v", err)
	}

	tuples := map[rbacTuple]bool{}
	identity := ""
	humanTable := false
	lastRole := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			identity, humanTable, lastRole = "", false, ""
			switch {
			case strings.HasPrefix(heading, "Dashboard ServiceAccount"):
				identity = dashboardIdentity
			case strings.HasPrefix(heading, "Operator ServiceAccount"):
				identity = operatorIdentity
			case strings.HasPrefix(heading, "Receiver ServiceAccount"),
				strings.HasPrefix(heading, "Build job ServiceAccount"):
				// Zero-permission identities: prose sections, no tuples.
			case strings.HasPrefix(heading, "Human roles"):
				humanTable = true
			default:
				t.Fatalf("rbac-matrix.md has an unrecognized section %q — teach this parser about it so coverage is not silently dropped", heading)
			}
			continue
		}
		if !strings.HasPrefix(line, "|") || (identity == "" && !humanTable) {
			continue
		}
		cells := splitTableRow(t, line)
		if isHeaderOrSeparatorRow(cells) {
			continue
		}
		rowIdentity := identity
		if humanTable {
			if len(cells) != 4 {
				t.Fatalf("human-roles row %q has %d columns, want 4", line, len(cells))
			}
			if cells[0] != "" {
				lastRole = cells[0]
			}
			if lastRole == "" {
				t.Fatalf("human-roles row %q has no role name and no preceding row to inherit one from", line)
			}
			rowIdentity = humanIdentityPrefix + lastRole
			cells = cells[1:]
		}
		if len(cells) != 3 {
			t.Fatalf("matrix row %q has %d columns, want 3", line, len(cells))
		}
		verbs := parseVerbsCell(t, cells[1])
		namespaces := parseScopeCell(t, cells[2])
		for _, gr := range parseResourcesCell(t, cells[0]) {
			for _, resource := range gr.resources {
				for _, verb := range verbs {
					for _, ns := range namespaces {
						tuples[rbacTuple{rowIdentity, ns, gr.group, resource, verb}] = true
					}
				}
			}
		}
	}
	if len(tuples) == 0 {
		t.Fatal("parsed zero tuples from rbac-matrix.md — the parser no longer understands the doc")
	}
	return tuples
}

func splitTableRow(t *testing.T, line string) []string {
	t.Helper()
	parts := strings.Split(line, "|")
	if len(parts) < 3 || parts[0] != "" || strings.TrimSpace(parts[len(parts)-1]) != "" {
		t.Fatalf("table row %q is not |-delimited as expected", line)
	}
	cells := parts[1 : len(parts)-1]
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func isHeaderOrSeparatorRow(cells []string) bool {
	if cells[0] == "Resource (API group)" || cells[0] == "Role" {
		return true
	}
	for _, c := range cells {
		if !separatorCellRe.MatchString(c) {
			return false
		}
	}
	return true
}

type groupResources struct {
	group     string
	resources []string
}

func parseResourcesCell(t *testing.T, cell string) []groupResources {
	t.Helper()
	var pairs []groupResources
	for _, clause := range strings.Split(cell, ";") {
		clause = strings.TrimSpace(clause)
		m := resourceClauseRe.FindStringSubmatch(clause)
		if m == nil {
			t.Fatalf("resources clause %q does not match `resources (group)`", clause)
		}
		group := m[2]
		switch group {
		case "core":
			group = ""
		case "authentication":
			// The doc's label for impersonation targets; users and groups
			// live in the core API group as far as RBAC is concerned.
			group = ""
		}
		resourcesPart := m[1]
		var suffixes []string
		if before, after, found := strings.Cut(resourcesPart, "+"); found {
			resourcesPart = before
			for _, s := range strings.Split(after, ",") {
				suffix := strings.Trim(strings.TrimSpace(s), "`")
				if !strings.HasPrefix(suffix, "/") {
					t.Fatalf("subresource suffix %q in clause %q must start with /", suffix, clause)
				}
				suffixes = append(suffixes, suffix)
			}
		}
		var resources []string
		for _, r := range strings.Split(resourcesPart, ",") {
			r = strings.Trim(strings.TrimSpace(r), "`")
			if r == "" {
				t.Fatalf("empty resource in clause %q", clause)
			}
			resources = append(resources, r)
			for _, suffix := range suffixes {
				resources = append(resources, r+suffix)
			}
		}
		pairs = append(pairs, groupResources{group: group, resources: resources})
	}
	return pairs
}

func parseVerbsCell(t *testing.T, cell string) []string {
	t.Helper()
	cell = strings.ReplaceAll(cell, "**", "")
	cell, _, _ = strings.Cut(cell, "—")
	var verbs []string
	for _, v := range strings.Split(cell, ",") {
		v = strings.TrimSpace(v)
		if !rbacKnownVerbs[v] {
			t.Fatalf("verbs cell %q contains %q, which is not a known RBAC verb", cell, v)
		}
		verbs = append(verbs, v)
	}
	return verbs
}

func parseScopeCell(t *testing.T, cell string) []string {
	t.Helper()
	if strings.Contains(cell, "cluster-scoped") {
		return []string{""}
	}
	var namespaces []string
	for _, m := range codeSpanRe.FindAllStringSubmatch(cell, -1) {
		if !orkanoNamespaces[m[1]] {
			t.Fatalf("scope cell %q names unknown namespace %q", cell, m[1])
		}
		namespaces = append(namespaces, m[1])
	}
	if len(namespaces) == 0 {
		t.Fatalf("scope cell %q names no namespace and is not cluster-scoped", cell)
	}
	return namespaces
}

// --- config/ manifest loading ---

type rbacManifests struct {
	namespaces          map[string]*corev1.Namespace
	serviceAccounts     map[string]*corev1.ServiceAccount
	roles               map[string]*rbacv1.Role
	roleBindings        []*rbacv1.RoleBinding
	clusterRoles        map[string]*rbacv1.ClusterRole
	clusterRoleBindings []*rbacv1.ClusterRoleBinding
	applyOrder          []*unstructured.Unstructured
}

func loadRBACManifests(t *testing.T) *rbacManifests {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	// Strict decoding so a misspelled field in a manifest (silently dropped
	// by the server) fails here instead.
	decoder := serializerjson.NewSerializerWithOptions(
		serializerjson.DefaultMetaFactory, scheme, scheme,
		serializerjson.SerializerOptions{Yaml: true, Strict: true},
	)

	m := &rbacManifests{
		namespaces:      map[string]*corev1.Namespace{},
		serviceAccounts: map[string]*corev1.ServiceAccount{},
		roles:           map[string]*rbacv1.Role{},
		clusterRoles:    map[string]*rbacv1.ClusterRole{},
	}
	for _, dir := range []string{"namespaces", "rbac"} {
		files, err := filepath.Glob(filepath.Join("..", "..", "..", "config", dir, "*.yaml"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no manifests under config/%s: %v", dir, err)
		}
		sort.Strings(files)
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("failed to read %s: %v", file, err)
			}
			reader := yamlutil.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
			for {
				doc, err := reader.Read()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("failed to split %s: %v", file, err)
				}
				if len(bytes.TrimSpace(doc)) == 0 {
					continue
				}
				obj, _, err := decoder.Decode(doc, nil, nil)
				if err != nil {
					t.Fatalf("failed to decode a document of %s: %v", file, err)
				}
				switch typed := obj.(type) {
				case *corev1.Namespace:
					m.namespaces[typed.Name] = typed
				case *corev1.ServiceAccount:
					m.serviceAccounts[typed.Namespace+"/"+typed.Name] = typed
				case *rbacv1.Role:
					m.roles[typed.Namespace+"/"+typed.Name] = typed
				case *rbacv1.RoleBinding:
					m.roleBindings = append(m.roleBindings, typed)
				case *rbacv1.ClusterRole:
					m.clusterRoles[typed.Name] = typed
				case *rbacv1.ClusterRoleBinding:
					m.clusterRoleBindings = append(m.clusterRoleBindings, typed)
				default:
					t.Fatalf("%s contains unexpected kind %T", file, obj)
				}
				u := &unstructured.Unstructured{}
				if err := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096).Decode(u); err != nil {
					t.Fatalf("failed to decode %s into unstructured: %v", file, err)
				}
				m.applyOrder = append(m.applyOrder, u)
			}
		}
	}
	return m
}

// manifestTuples expands Roles through their Bindings into the same tuple
// space the doc parser produces. Unbound Roles must be the doc's human roles;
// anything else in config/ that grants permissions fails the test.
func manifestTuples(t *testing.T, m *rbacManifests, humanRoles map[string]bool) map[rbacTuple]bool {
	t.Helper()
	tuples := map[rbacTuple]bool{}
	bound := map[string]bool{}

	expand := func(identity, namespace string, rules []rbacv1.PolicyRule, where string) {
		for _, rule := range rules {
			if len(rule.ResourceNames) > 0 || len(rule.NonResourceURLs) > 0 {
				t.Fatalf("%s uses resourceNames/nonResourceURLs, which the matrix doc cannot express", where)
			}
			for _, group := range rule.APIGroups {
				for _, resource := range rule.Resources {
					for _, verb := range rule.Verbs {
						tuples[rbacTuple{identity, namespace, group, resource, verb}] = true
					}
				}
			}
		}
	}
	subjectIdentity := func(s rbacv1.Subject, where string) string {
		if s.Kind != rbacv1.ServiceAccountKind {
			t.Fatalf("%s binds subject kind %q; config/ ships ServiceAccount bindings only", where, s.Kind)
		}
		if _, ok := m.serviceAccounts[s.Namespace+"/"+s.Name]; !ok {
			t.Fatalf("%s binds ServiceAccount %s/%s, which no manifest defines", where, s.Namespace, s.Name)
		}
		return "system:serviceaccount:" + s.Namespace + ":" + s.Name
	}

	for _, rb := range m.roleBindings {
		where := fmt.Sprintf("RoleBinding %s/%s", rb.Namespace, rb.Name)
		if rb.RoleRef.Kind != "Role" {
			t.Fatalf("%s references kind %q; config/ ships Role references only", where, rb.RoleRef.Kind)
		}
		role, ok := m.roles[rb.Namespace+"/"+rb.RoleRef.Name]
		if !ok {
			t.Fatalf("%s references Role %q, which no manifest defines", where, rb.RoleRef.Name)
		}
		bound[rb.Namespace+"/"+rb.RoleRef.Name] = true
		for _, s := range rb.Subjects {
			expand(subjectIdentity(s, where), rb.Namespace, role.Rules, where)
		}
	}
	for _, crb := range m.clusterRoleBindings {
		where := fmt.Sprintf("ClusterRoleBinding %s", crb.Name)
		if crb.RoleRef.Kind != "ClusterRole" {
			t.Fatalf("%s references kind %q, want ClusterRole", where, crb.RoleRef.Kind)
		}
		clusterRole, ok := m.clusterRoles[crb.RoleRef.Name]
		if !ok {
			t.Fatalf("%s references ClusterRole %q, which no manifest defines", where, crb.RoleRef.Name)
		}
		bound["clusterrole/"+crb.RoleRef.Name] = true
		for _, s := range crb.Subjects {
			expand(subjectIdentity(s, where), "", clusterRole.Rules, where)
		}
	}
	for key, role := range m.roles {
		if bound[key] {
			continue
		}
		if !humanRoles[role.Name] {
			t.Fatalf("Role %s is unbound and is not a human role the matrix doc knows about", key)
		}
		expand(humanIdentityPrefix+role.Name, role.Namespace, role.Rules, "Role "+key)
	}
	for name := range m.clusterRoles {
		if !bound["clusterrole/"+name] {
			t.Fatalf("ClusterRole %s is unbound — nothing in the matrix doc grants it", name)
		}
	}
	return tuples
}

func sortedTuples(set map[rbacTuple]bool) []rbacTuple {
	out := make([]rbacTuple, 0, len(set))
	for tu := range set {
		out = append(out, tu)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func TestRBACMatrixDocMatchesManifests(t *testing.T) {
	docTuples := parseRBACMatrixDoc(t)
	manifests := loadRBACManifests(t)
	humanRoles := map[string]bool{}
	for tu := range docTuples {
		if name, ok := strings.CutPrefix(tu.identity, humanIdentityPrefix); ok {
			humanRoles[name] = true
		}
	}
	manTuples := manifestTuples(t, manifests, humanRoles)

	for _, tu := range sortedTuples(docTuples) {
		if !manTuples[tu] {
			t.Errorf("rbac-matrix.md grants {%s} but no manifest does", tu)
		}
	}
	for _, tu := range sortedTuples(manTuples) {
		if !docTuples[tu] {
			t.Errorf("manifests grant {%s} but rbac-matrix.md does not — update the doc or drop the grant", tu)
		}
	}
}

func TestNamespacePSALabels(t *testing.T) {
	// orkano-builds is pinned at baseline by ADR-0012; warn/audit follow
	// because restricted would flag every legitimate build pod. orkano-system
	// holds only Orkano components, non-root and read-only per ADR-0007.
	// orkano-apps runs arbitrary user images, so enforce stays at baseline
	// with warn/audit surfacing the restricted gap.
	expected := map[string]map[string]string{
		systemNamespace: {"enforce": "restricted", "warn": "restricted", "audit": "restricted"},
		appsNamespace:   {"enforce": "baseline", "warn": "restricted", "audit": "restricted"},
		buildsNamespace: {"enforce": "baseline", "warn": "baseline", "audit": "baseline"},
	}
	manifests := loadRBACManifests(t)
	if len(manifests.namespaces) != len(expected) {
		t.Errorf("config/namespaces defines %d namespaces, want %d", len(manifests.namespaces), len(expected))
	}
	for name, levels := range expected {
		ns, ok := manifests.namespaces[name]
		if !ok {
			t.Errorf("namespace %s missing from config/namespaces", name)
			continue
		}
		for axis, level := range levels {
			label := "pod-security.kubernetes.io/" + axis
			if got := ns.Labels[label]; got != level {
				t.Errorf("namespace %s label %s = %q, want %q", name, label, got, level)
			}
		}
	}
}

func TestZeroPermissionServiceAccounts(t *testing.T) {
	manifests := loadRBACManifests(t)
	expected := map[string]bool{ // key → automountServiceAccountToken must be false
		"orkano-system/orkano-operator":  false,
		"orkano-system/orkano-dashboard": false,
		"orkano-system/orkano-receiver":  true,
		"orkano-builds/orkano-build":     true,
	}
	if len(manifests.serviceAccounts) != len(expected) {
		t.Errorf("config/rbac defines %d ServiceAccounts, want %d", len(manifests.serviceAccounts), len(expected))
	}
	for key, mustDisableAutomount := range expected {
		sa, ok := manifests.serviceAccounts[key]
		if !ok {
			t.Errorf("ServiceAccount %s missing from config/rbac", key)
			continue
		}
		if mustDisableAutomount && (sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken) {
			t.Errorf("ServiceAccount %s must set automountServiceAccountToken: false", key)
		}
	}
}

// --- behavioral walk via SubjectAccessReview ---

func sarAllowed(t *testing.T, ctx context.Context, user, namespace, group, resource, verb string) bool {
	t.Helper()
	res, sub, _ := strings.Cut(resource, "/")
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User: user,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        verb,
				Group:       group,
				Resource:    res,
				Subresource: sub,
			},
		},
	}
	if err := k8sClient.Create(ctx, sar); err != nil {
		t.Fatalf("SubjectAccessReview for %s failed: %v", user, err)
	}
	return sar.Status.Allowed
}

func sarUser(identity string) string {
	if name, ok := strings.CutPrefix(identity, humanIdentityPrefix); ok {
		return "sar-probe:" + name
	}
	return identity
}

func TestRBACMatrixSubjectAccessReviews(t *testing.T) {
	ctx := context.Background()
	docTuples := parseRBACMatrixDoc(t)
	manifests := loadRBACManifests(t)

	for _, obj := range manifests.applyOrder {
		if err := k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
			client.FieldOwner("rbac-matrix-test"), client.ForceOwnership); err != nil {
			t.Fatalf("failed to apply %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	for name := range orkanoNamespaces {
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
			t.Fatalf("failed to read namespace %s after apply: %v", name, err)
		}
		want := manifests.namespaces[name].Labels
		for label, level := range want {
			if ns.Labels[label] != level {
				t.Errorf("namespace %s label %s = %q in the cluster, want %q", name, label, ns.Labels[label], level)
			}
		}
	}

	// Human roles ship unbound; bind a probe user per role so the authorizer
	// can be asked about them.
	humanRoles := map[string]bool{}
	for tu := range docTuples {
		if name, ok := strings.CutPrefix(tu.identity, humanIdentityPrefix); ok {
			humanRoles[name] = true
		}
	}
	for name := range humanRoles {
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "sar-probe-" + name, Namespace: appsNamespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "sar-probe:" + name}},
		}
		if err := k8sClient.Create(ctx, rb); err != nil {
			t.Fatalf("failed to bind probe user for role %s: %v", name, err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rb) })
	}

	// The authorizer reads RBAC through informers; wait for one canary tuple
	// per binding before walking, so the walk itself is deterministic.
	canaries := []rbacTuple{
		{dashboardIdentity, appsNamespace, "orkano.io", "apps", "get"},
		{dashboardIdentity, "", "", "users", "impersonate"},
		{operatorIdentity, appsNamespace, "apps", "deployments", "get"},
		{operatorIdentity, buildsNamespace, "batch", "jobs", "get"},
		{operatorIdentity, systemNamespace, "coordination.k8s.io", "leases", "get"},
	}
	for name := range humanRoles {
		canaries = append(canaries, rbacTuple{humanIdentityPrefix + name, appsNamespace, "orkano.io", "apps", "get"})
	}
	for _, canary := range canaries {
		eventually(t, fmt.Sprintf("authorizer to allow canary {%s}", canary), func(ctx context.Context) (bool, error) {
			return sarAllowed(t, ctx, sarUser(canary.identity), canary.namespace, canary.group, canary.resource, canary.verb), nil
		})
	}

	for _, tu := range sortedTuples(docTuples) {
		if !sarAllowed(t, ctx, sarUser(tu.identity), tu.namespace, tu.group, tu.resource, tu.verb) {
			t.Errorf("matrix grants {%s} but the authorizer denies it", tu)
		}
	}

	// The denied walk: every combination of the identities, the doc's
	// resource universe plus high-value canaries, every verb, every scope —
	// minus what the doc grants — must be denied. A cluster-scoped grant
	// (impersonation) also authorizes namespaced requests, so it suppresses
	// its namespaced combinations.
	identities := []string{dashboardIdentity, operatorIdentity, receiverIdentity, buildIdentity}
	for name := range humanRoles {
		identities = append(identities, humanIdentityPrefix+name)
	}
	type groupResource struct{ group, resource string }
	universe := map[groupResource]bool{
		{"", "pods/exec"}:                             true,
		{"", "serviceaccounts"}:                       true,
		{"", "nodes"}:                                 true,
		{"rbac.authorization.k8s.io", "roles"}:        true,
		{"rbac.authorization.k8s.io", "rolebindings"}: true,
	}
	clusterGrants := map[string]map[groupResource]map[string]bool{}
	for tu := range docTuples {
		universe[groupResource{tu.group, tu.resource}] = true
		if tu.namespace == "" {
			if clusterGrants[tu.identity] == nil {
				clusterGrants[tu.identity] = map[groupResource]map[string]bool{}
			}
			gr := groupResource{tu.group, tu.resource}
			if clusterGrants[tu.identity][gr] == nil {
				clusterGrants[tu.identity][gr] = map[string]bool{}
			}
			clusterGrants[tu.identity][gr][tu.verb] = true
		}
	}
	verbs := make([]string, 0, len(rbacKnownVerbs))
	for v := range rbacKnownVerbs {
		verbs = append(verbs, v)
	}
	scopes := []string{appsNamespace, buildsNamespace, systemNamespace, ""}
	denied, walked := 0, 0
	for _, identity := range identities {
		for gr := range universe {
			for _, verb := range verbs {
				for _, scope := range scopes {
					tu := rbacTuple{identity, scope, gr.group, gr.resource, verb}
					if docTuples[tu] || clusterGrants[identity][gr][verb] {
						continue
					}
					walked++
					if sarAllowed(t, ctx, sarUser(identity), scope, gr.group, gr.resource, verb) {
						t.Errorf("nothing in the matrix grants {%s}, but the authorizer allows it", tu)
						continue
					}
					denied++
				}
			}
		}
	}
	t.Logf("denied walk: %d/%d combinations correctly denied", denied, walked)
}
