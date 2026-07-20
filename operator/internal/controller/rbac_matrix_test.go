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
	"encoding/base64"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	serializerjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// systemNamespace/appsNamespace/buildNamespace are owned by cache.go.
	dashboardIdentity = "system:serviceaccount:orkano-system:orkano-dashboard"
	operatorIdentity  = "system:serviceaccount:orkano-system:orkano-operator"
	receiverIdentity  = "system:serviceaccount:orkano-system:orkano-receiver"
	buildIdentity     = "system:serviceaccount:orkano-builds:orkano-build"

	humanIdentityPrefix = "role:"
)

// rbacTuple is one cell of the matrix: identity may do verb on group/resource
// in namespace, optionally pinned to one object by resourceName ("" means
// unrestricted). namespace "" means cluster-scoped. Human roles use the
// identity "role:<name>" because they ship unbound — bindings to real OIDC
// identities are created at install time, not in config/.
type rbacTuple struct {
	identity     string
	namespace    string
	group        string
	resource     string
	resourceName string
	verb         string
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
	resource := tu.resource
	if tu.resourceName != "" {
		resource += "[" + tu.resourceName + "]"
	}
	return fmt.Sprintf("%s: %s %s/%s in %s", tu.identity, tu.verb, group, resource, ns)
}

var rbacKnownVerbs = map[string]bool{
	"get": true, "list": true, "watch": true, "create": true,
	"update": true, "patch": true, "delete": true, "impersonate": true,
}

// Collection requests carry no object name, so a rule combining resourceNames
// with these verbs is dead under the real authorizer — yet a SubjectAccessReview
// can be handed a name the request would never have, so the SAR walk alone
// cannot expose the mistake. Refuse it structurally instead.
var rbacCollectionVerbs = map[string]bool{"list": true, "watch": true, "create": true}

var orkanoNamespaces = map[string]bool{
	systemNamespace: true, appsNamespace: true, buildNamespace: true,
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
	zeroPermSection := false
	lastRole := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			identity, humanTable, zeroPermSection, lastRole = "", false, false, ""
			switch {
			case strings.HasPrefix(heading, "Dashboard ServiceAccount"):
				identity = dashboardIdentity
			case strings.HasPrefix(heading, "Operator ServiceAccount"):
				identity = operatorIdentity
			case strings.HasPrefix(heading, "Receiver ServiceAccount"),
				strings.HasPrefix(heading, "Build job ServiceAccount"):
				// Zero-permission identities: prose sections, no tuples. A
				// permission table appearing here is doc drift the manifests
				// would never reflect — fail rather than silently ignore it.
				zeroPermSection = true
			case strings.HasPrefix(heading, "Human roles"):
				humanTable = true
			default:
				t.Fatalf("rbac-matrix.md has an unrecognized section %q — teach this parser about it so coverage is not silently dropped", heading)
			}
			continue
		}
		if zeroPermSection && strings.HasPrefix(line, "|") {
			t.Fatalf("rbac-matrix.md has a table row under a zero-permission section but those identities hold no grants: %q", line)
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
			for _, verb := range verbs {
				if gr.resourceName != "" && rbacCollectionVerbs[verb] {
					t.Fatalf("matrix row %q grants %q on %s[%s], but resourceNames can never authorize a collection request", line, verb, gr.resource, gr.resourceName)
				}
				for _, ns := range namespaces {
					tuples[rbacTuple{rowIdentity, ns, gr.group, gr.resource, gr.resourceName, verb}] = true
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

// matrixResource is one (group, resource) the doc grants, optionally pinned to
// a single object by resourceName via the `resource[name]` syntax.
type matrixResource struct {
	group        string
	resource     string
	resourceName string
}

func parseResourcesCell(t *testing.T, cell string) []matrixResource {
	t.Helper()
	var out []matrixResource
	for _, clause := range strings.Split(cell, ";") {
		clause = strings.TrimSpace(clause)
		m := resourceClauseRe.FindStringSubmatch(clause)
		if m == nil {
			t.Fatalf("resources clause %q does not match `resources (group)`", clause)
		}
		group := m[2]
		// Phase 2's impersonation row must label users/groups "(core)" —
		// that is their API group as far as RBAC is concerned.
		if group == "core" {
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
		for _, r := range strings.Split(resourcesPart, ",") {
			r = strings.Trim(strings.TrimSpace(r), "`")
			if r == "" {
				t.Fatalf("empty resource in clause %q", clause)
			}
			resource, resourceName := r, ""
			if base, name, found := strings.Cut(r, "["); found {
				if !strings.HasSuffix(name, "]") || name == "]" {
					t.Fatalf("resource %q in clause %q has malformed [resourceName]", r, clause)
				}
				if len(suffixes) > 0 {
					t.Fatalf("resource %q in clause %q mixes a [resourceName] with a subresource suffix; the matrix cannot express that", r, clause)
				}
				resource = base
				resourceName = strings.TrimSuffix(name, "]")
			}
			out = append(out, matrixResource{group: group, resource: resource, resourceName: resourceName})
			for _, suffix := range suffixes {
				out = append(out, matrixResource{group: group, resource: resource + suffix})
			}
		}
	}
	return out
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
		// List the whole directory rather than glob *.yaml: a stray .yml or
		// .json that `kubectl apply -f config/rbac/` would deploy must fail
		// the test, not slip past both the structural compare and the SAR
		// walk. Only .yaml is permitted; anything else is a loud error.
		entries, err := os.ReadDir(filepath.Join("..", "..", "..", "config", dir))
		if err != nil || len(entries) == 0 {
			t.Fatalf("no manifests under config/%s: %v", dir, err)
		}
		var files []string
		for _, e := range entries {
			if e.IsDir() {
				t.Fatalf("config/%s/%s is a directory; RBAC manifests must be flat .yaml files", dir, e.Name())
			}
			if filepath.Ext(e.Name()) != ".yaml" {
				t.Fatalf("config/%s/%s is not a .yaml file; kubectl apply would deploy it but this test would not see it", dir, e.Name())
			}
			files = append(files, filepath.Join("..", "..", "..", "config", dir, e.Name()))
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
					// An aggregationRule's effective rules are filled in at
					// runtime by the cluster-role-aggregation controller, which
					// envtest does not run — so the rules read as empty here and
					// the role expands to zero tuples, passing the structural
					// compare while granting (e.g. aggregate-to-admin) in a real
					// cluster. Refuse to load such a role rather than vouch for it.
					if typed.AggregationRule != nil {
						t.Fatalf("ClusterRole %s carries an aggregationRule; its effective permissions cannot be statically verified", typed.Name)
					}
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
			if len(rule.NonResourceURLs) > 0 {
				t.Fatalf("%s uses nonResourceURLs, which the matrix doc cannot express", where)
			}
			// resourceNames pin a rule to named objects; the matrix expresses
			// that as `resource[name]`. A collection verb under such a rule is
			// dead (List/Watch/Create requests carry no name), so refuse it —
			// same guard the doc parser applies, kept symmetric here.
			names := rule.ResourceNames
			if len(names) == 0 {
				names = []string{""}
			} else {
				for _, verb := range rule.Verbs {
					if rbacCollectionVerbs[verb] {
						t.Fatalf("%s grants collection verb %q on a resourceNames-pinned rule, which can never authorize", where, verb)
					}
				}
			}
			for _, group := range rule.APIGroups {
				for _, resource := range rule.Resources {
					for _, name := range names {
						for _, verb := range rule.Verbs {
							tuples[rbacTuple{identity, namespace, group, resource, name, verb}] = true
						}
					}
				}
			}
		}
	}
	subjectIdentity := func(s rbacv1.Subject, where, roleName string) string {
		switch s.Kind {
		case rbacv1.ServiceAccountKind:
			if _, ok := m.serviceAccounts[s.Namespace+"/"+s.Name]; !ok {
				t.Fatalf("%s binds ServiceAccount %s/%s, which no manifest defines", where, s.Namespace, s.Name)
			}
			return "system:serviceaccount:" + s.Namespace + ":" + s.Name
		case rbacv1.GroupKind:
			// config/ ships exactly one Group subject: the dashboard's fixed viewer
			// group bound to the read-only orkano-viewer Role (ADR-0015). Pin the
			// name so a renamed or additional Group binding fails loudly here rather
			// than slipping past the doc<->manifest equality, which keys a group
			// binding on role:<name> and so would not otherwise notice the subject.
			const viewerGroupSubject = "orkano:viewers"
			if s.Name != viewerGroupSubject {
				t.Fatalf("%s binds Group %q; config/ ships only the fixed viewer group %q as a Group subject", where, s.Name, viewerGroupSubject)
			}
			// Model its permissions under the role:<name> identity the matrix uses
			// for human roles, so the doc's human-role rows still match. The SAR walk
			// separately binds a sar-probe user per human role, and a dedicated check
			// below proves the group binding itself authorizes.
			return humanIdentityPrefix + roleName
		default:
			t.Fatalf("%s binds subject kind %q; config/ ships ServiceAccount and Group bindings only", where, s.Kind)
			return ""
		}
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
			expand(subjectIdentity(s, where, rb.RoleRef.Name), rb.Namespace, role.Rules, where)
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
			expand(subjectIdentity(s, where, crb.RoleRef.Name), "", clusterRole.Rules, where)
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
		buildNamespace:  {"enforce": "baseline", "warn": "baseline", "audit": "baseline"},
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

func sarAllowed(t *testing.T, ctx context.Context, user, namespace, group, resource, resourceName, verb string) bool {
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
				Name:        resourceName,
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

// sarAllowedAs is sarAllowed for a request from a user IN the given groups —
// used to prove a Group binding (the dashboard's impersonated viewer group)
// actually authorizes its Role's permissions.
func sarAllowedAs(t *testing.T, ctx context.Context, user string, groups []string, namespace, group, resource, verb string) bool {
	t.Helper()
	res, sub, _ := strings.Cut(resource, "/")
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   user,
			Groups: groups,
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
		t.Fatalf("SubjectAccessReview for groups %v failed: %v", groups, err)
	}
	return sar.Status.Allowed
}

func TestRBACMatrixSubjectAccessReviews(t *testing.T) {
	ctx := context.Background()
	docTuples := parseRBACMatrixDoc(t)
	manifests := loadRBACManifests(t)

	// suite_test.go runs one apiserver + manager for the whole package and
	// pre-creates orkano-system/orkano-apps unlabeled. Applying PSA labels here
	// changes admission for every test that runs afterward (envtest keeps
	// PodSecurity admission live), so undo it on cleanup: strip the labels we
	// add and delete the RBAC objects. Namespaces are not deleted — envtest
	// runs no namespace controller, so a deleted namespace would wedge in
	// Terminating; stripping labels restores the permissive pre-test state.
	for _, obj := range manifests.applyOrder {
		if err := k8sClient.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
			client.FieldOwner("rbac-matrix-test"), client.ForceOwnership); err != nil {
			t.Fatalf("failed to apply %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
		t.Cleanup(func() {
			bg := context.Background()
			if obj.GetKind() == "Namespace" {
				patch := []byte(`{"metadata":{"labels":{` +
					`"pod-security.kubernetes.io/enforce":null,` +
					`"pod-security.kubernetes.io/warn":null,` +
					`"pod-security.kubernetes.io/audit":null}}}`)
				_ = k8sClient.Patch(bg, obj, client.RawPatch(types.MergePatchType, patch))
				return
			}
			_ = k8sClient.Delete(bg, obj)
		})
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

	// Human roles may span namespaces; bind a probe user to each namespaced Role
	// named by the matrix so the authorizer can be asked about every tuple.
	humanRoles := map[string]bool{}
	humanRoleNamespaces := map[string]map[string]bool{}
	for tu := range docTuples {
		if name, ok := strings.CutPrefix(tu.identity, humanIdentityPrefix); ok {
			humanRoles[name] = true
			if humanRoleNamespaces[name] == nil {
				humanRoleNamespaces[name] = map[string]bool{}
			}
			humanRoleNamespaces[name][tu.namespace] = true
		}
	}
	for name, namespaces := range humanRoleNamespaces {
		for namespace := range namespaces {
			rb := &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "sar-probe-" + name, Namespace: namespace},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "sar-probe:" + name}},
			}
			if err := k8sClient.Create(ctx, rb); err != nil {
				t.Fatalf("failed to bind probe user for role %s in %s: %v", name, namespace, err)
			}
			t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), rb) })
		}
	}

	// The authorizer reads RBAC through informers; wait for one canary tuple
	// per binding before walking, so the walk itself is deterministic.
	canaries := []rbacTuple{
		{identity: dashboardIdentity, namespace: appsNamespace, group: "orkano.io", resource: "apps", verb: "get"},
		{identity: dashboardIdentity, namespace: appsNamespace, resource: "secrets", verb: "create"},
		{identity: operatorIdentity, namespace: appsNamespace, group: "apps", resource: "deployments", verb: "get"},
		{identity: operatorIdentity, namespace: buildNamespace, group: "batch", resource: "jobs", verb: "get"},
		{identity: operatorIdentity, namespace: systemNamespace, group: "coordination.k8s.io", resource: "leases", verb: "get"},
		{identity: dashboardIdentity, namespace: "", group: "", resource: "groups", resourceName: "orkano:viewers", verb: "impersonate"},
		// The dashboard's orkano-system credential-write grant (orkano-dashboard-credentials).
		{identity: dashboardIdentity, namespace: systemNamespace, resource: "secrets", resourceName: "orkano-github-app", verb: "update"},
	}
	for name := range humanRoles {
		canaries = append(canaries, rbacTuple{identity: humanIdentityPrefix + name, namespace: appsNamespace, group: "orkano.io", resource: "apps", verb: "get"})
	}
	for _, canary := range canaries {
		eventually(t, fmt.Sprintf("authorizer to allow canary {%s}", canary), func(ctx context.Context) (bool, error) {
			return sarAllowed(t, ctx, sarUser(canary.identity), canary.namespace, canary.group, canary.resource, canary.resourceName, canary.verb), nil
		})
	}

	for _, tu := range sortedTuples(docTuples) {
		if !sarAllowed(t, ctx, sarUser(tu.identity), tu.namespace, tu.group, tu.resource, tu.resourceName, tu.verb) {
			t.Errorf("matrix grants {%s} but the authorizer denies it", tu)
		}
	}

	// The registry-rotation grant is pinned by resourceNames to orkano-registry:
	// prove the pin actually binds by naming a peer Deployment the operator must
	// never touch (the dashboard/receiver live in orkano-system too). The denied
	// walk already covers the nameless request; this covers the wrong-name one.
	for _, verb := range []string{"get", "update"} {
		if sarAllowed(t, ctx, operatorIdentity, systemNamespace, "apps", "deployments", "orkano-dashboard", verb) {
			t.Errorf("operator may %s deployments/orkano-dashboard in orkano-system, but the resourceNames pin should deny it", verb)
		}
	}

	// The dashboard's orkano-system secret-write grant is pinned by resourceNames to
	// exactly orkano-github-app + orkano-webhook-secret + orkano-oidc; prove the pin
	// binds by naming the most sensitive co-residents it must never reach (and that
	// update is the only verb, never get/create/delete). The denied walk covers only
	// the nameless request.
	for _, bad := range []string{"orkano-postgres-superuser", "orkano-dashboard-enc-key", "orkano-bootstrap-token"} {
		if sarAllowed(t, ctx, dashboardIdentity, systemNamespace, "", "secrets", bad, "update") {
			t.Errorf("dashboard may update secrets/%s in orkano-system, but the resourceNames pin should deny it", bad)
		}
	}
	for _, pinned := range []string{"orkano-github-app", "orkano-webhook-secret", "orkano-oidc"} {
		for _, verb := range []string{"get", "create", "delete", "patch"} {
			if sarAllowed(t, ctx, dashboardIdentity, systemNamespace, "", "secrets", pinned, verb) {
				t.Errorf("dashboard may %s secrets/%s in orkano-system, but only update is granted", verb, pinned)
			}
		}
	}

	// The dashboard's impersonate grant is pinned by resourceNames to exactly one
	// fixed user and one fixed group; prove the pins bind by naming identities it
	// must never impersonate — nameless, a privileged group, and the cross-pin
	// (the user pin is orkano:viewer, the group pin orkano:viewers). The denied
	// walk suppresses these because a cluster grant authorizes every scope but its
	// key ignores resourceName, so this explicit wrong-name probe is the real guard.
	for _, bad := range []struct{ resource, name string }{
		{"users", ""}, {"users", "system:masters"}, {"users", "orkano:viewers"},
		{"groups", ""}, {"groups", "system:masters"}, {"groups", "orkano:viewer"},
	} {
		if sarAllowed(t, ctx, dashboardIdentity, "", "", bad.resource, bad.name, "impersonate") {
			t.Errorf("dashboard may impersonate %s/%q, but only the pinned viewer identity is allowed", bad.resource, bad.name)
		}
	}

	// The shipped orkano:viewers -> orkano-viewer binding makes the impersonated
	// reads work out of the box: wait for the group binding to authorize a read...
	eventually(t, "the orkano:viewers group binding to authorize a read", func(ctx context.Context) (bool, error) {
		return sarAllowedAs(t, ctx, "orkano:viewer", []string{"orkano:viewers"}, appsNamespace, "orkano.io", "apps", "get"), nil
	})
	// ...but the group is read-only, so it cannot mutate.
	if sarAllowedAs(t, ctx, "orkano:viewer", []string{"orkano:viewers"}, appsNamespace, "orkano.io", "apps", "delete") {
		t.Error("a member of orkano:viewers may delete apps, but orkano-viewer is read-only")
	}

	// The denied walk: every combination of the identities, the doc's
	// resource universe plus high-value canaries, every verb, every scope —
	// minus what the doc grants — must be denied. A cluster-scoped grant
	// also authorizes namespaced requests, so it suppresses its namespaced
	// combinations (no such grant exists since ADR-0013; the suppression
	// stays for when one returns).
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
		// Impersonation was dropped from the dashboard by ADR-0013 because an
		// unrestricted impersonate can name system:masters; the walk keeps
		// probing it so the grant cannot quietly return without resourceNames.
		{"", "users"}:  true,
		{"", "groups"}: true,
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
	scopes := []string{appsNamespace, buildNamespace, systemNamespace, ""}
	denied, walked := 0, 0
	for _, identity := range identities {
		for gr := range universe {
			for _, verb := range verbs {
				for _, scope := range scopes {
					tu := rbacTuple{identity: identity, namespace: scope, group: gr.group, resource: gr.resource, verb: verb}
					if docTuples[tu] || clusterGrants[identity][gr][verb] {
						continue
					}
					// resourceName left empty: a name-restricted grant (e.g.
					// deployments[orkano-registry]) must NOT authorize a
					// nameless request, so probing without a name proves the
					// restriction actually binds.
					walked++
					if sarAllowed(t, ctx, sarUser(identity), scope, gr.group, gr.resource, "", verb) {
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

// TestSecretVerbValueBlindness pins the apiserver behavior ADR-0013 rests on:
// of the Secret mutation verbs, create and update respond with nothing beyond
// the caller's own payload, while patch returns the stored object — values
// included — even when the patch touches only a label. The dashboard's secret
// grant is exactly the value-blind pair; if a Kubernetes upgrade changes any
// of these response shapes, this fails and the verb set must be reconsidered.
func TestSecretVerbValueBlindness(t *testing.T) {
	ctx := context.Background()
	const sentinel = "verb-probe-sentinel-3c9d"
	encoded := base64.StdEncoding.EncodeToString([]byte(sentinel))

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "verb-probe", Namespace: appsNamespace},
		StringData: map[string]string{"password": sentinel},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("failed to create probe Secret: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), secret) })

	cfg := rest.CopyConfig(restConfig)
	cfg.GroupVersion = &schema.GroupVersion{Version: "v1"}
	cfg.APIPath = "/api"
	cfg.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatalf("failed to build REST client: %v", err)
	}

	raw, err := rc.Patch(types.MergePatchType).Resource("secrets").Namespace(appsNamespace).Name(secret.Name).
		Body([]byte(`{"metadata":{"labels":{"probe":"x"}}}`)).DoRaw(ctx)
	if err != nil {
		t.Fatalf("PATCH failed: %v", err)
	}
	if !strings.Contains(string(raw), encoded) {
		t.Errorf("a label-only PATCH no longer returns stored values — patch may have become grantable, revisit ADR-0013")
	}

	raw, err = rc.Post().Resource("secrets").Namespace(appsNamespace).
		Body([]byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"` + secret.Name + `","namespace":"` + appsNamespace + `"},"stringData":{"k":"v"}}`)).
		DoRaw(ctx)
	if !apierrors.IsAlreadyExists(err) {
		t.Fatalf("expected AlreadyExists from CREATE on an existing Secret, got: %v", err)
	}
	if strings.Contains(string(raw), encoded) {
		t.Errorf("CREATE conflict response leaks stored values: %s", raw)
	}

	raw, err = rc.Put().Resource("secrets").Namespace(appsNamespace).Name(secret.Name).
		Body([]byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"` + secret.Name + `","namespace":"` + appsNamespace + `"},"stringData":{"rotated":"replacement"}}`)).
		DoRaw(ctx)
	if err != nil {
		t.Fatalf("PUT failed: %v", err)
	}
	if strings.Contains(string(raw), encoded) {
		t.Errorf("UPDATE response leaks the pre-update value — the blind-overwrite premise of ADR-0013 no longer holds")
	}
}
