package githubapp

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testSHA  = "0123456789abcdef0123456789abcdef01234567"
	testRepo = "orkanoio/orkano"
)

// repoStub answers the three endpoints ResolveCommit touches: the App-JWT
// installation lookup, the installation-token mint, and the token-authed repo /
// commit reads. It records which auth each endpoint saw so the test can prove
// the App JWT is used only where it must be and the installation token elsewhere.
type repoStub struct {
	t   *testing.T
	pub *rsa.PublicKey
	now func() time.Time

	defaultBranch string
	commitSHA     string

	// per-endpoint status overrides (0 = the success status)
	installationStatus int
	repoStatus         int
	commitStatus       int

	mu               sync.Mutex
	paths            []string
	installationAuth string
	repoAuth         string
	commitAuth       string
	commitRef        string
}

func (s *repoStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	authz := r.Header.Get("Authorization")

	// Route order is load-bearing: the "/commits/" check must precede the bare
	// "/repos/" prefix, since a commit path also starts with "/repos/".
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		if !s.requireAppJWT(w, authz) {
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_test_token",
			"expires_at": s.now().Add(time.Hour).Format(time.RFC3339),
		})

	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
		s.mu.Lock()
		s.installationAuth = authz
		s.mu.Unlock()
		if !s.requireAppJWT(w, authz) {
			return
		}
		if s.installationStatus != 0 {
			http.Error(w, `{"message":"not found"}`, s.installationStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": testInstallationID})

	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/commits/"):
		s.mu.Lock()
		s.commitAuth = authz
		s.commitRef = strings.SplitN(r.URL.Path, "/commits/", 2)[1]
		s.mu.Unlock()
		if s.commitStatus != 0 {
			http.Error(w, `{"message":"no commit"}`, s.commitStatus)
			return
		}
		sha := s.commitSHA
		if sha == "" {
			sha = testSHA
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha})

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/"):
		s.mu.Lock()
		s.repoAuth = authz
		s.mu.Unlock()
		if s.repoStatus != 0 {
			http.Error(w, `{"message":"not found"}`, s.repoStatus)
			return
		}
		branch := s.defaultBranch
		if branch == "" {
			branch = "main"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": branch})

	default:
		s.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}
}

// requireAppJWT verifies the Authorization is a Bearer App JWT this key signed.
// Runs in the server goroutine: failures use Errorf + a 500, never Fatalf.
func (s *repoStub) requireAppJWT(w http.ResponseWriter, authz string) bool {
	s.t.Helper()
	if !strings.HasPrefix(authz, "Bearer ") {
		s.t.Errorf("Authorization = %q, want a Bearer App JWT", authz)
		http.Error(w, "missing bearer", http.StatusInternalServerError)
		return false
	}
	if !verifyRS256(s.pub, strings.TrimPrefix(authz, "Bearer ")) {
		s.t.Errorf("Authorization is not a JWT signed by the App key: %q", authz)
		http.Error(w, "bad jwt", http.StatusInternalServerError)
		return false
	}
	return true
}

// verifyRS256 reports whether token is a three-segment RS256 JWT whose signature
// verifies against pub (an App JWT, distinguishing it from the opaque
// installation token used on the data endpoints).
func verifyRS256(pub *rsa.PublicKey, token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) == nil
}

func newResolver(t *testing.T, key *rsa.PrivateKey, stub *repoStub) *TokenSource {
	t.Helper()
	stub.t = t
	stub.pub = &key.PublicKey
	stub.now = func() time.Time { return fixedTime }
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	return &TokenSource{
		Reader:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(pkcs1Secret(t, key, strconv.Itoa(testAppID))).Build(),
		BaseURL: srv.URL,
		Now:     func() time.Time { return fixedTime },
	}
}

func TestResolveCommitDefaultBranch(t *testing.T) {
	key := mustKey(t)
	stub := &repoStub{defaultBranch: "trunk", commitSHA: testSHA}
	src := newResolver(t, key, stub)

	// Empty ref must trigger a default-branch lookup, then resolve that branch.
	sha, err := src.ResolveCommit(context.Background(), testRepo, "")
	if err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	if sha != testSHA {
		t.Fatalf("sha = %q, want %q", sha, testSHA)
	}

	// The repo (default-branch) endpoint must have been consulted, and the
	// commit must have been resolved at the discovered branch, not at "".
	if !contains(stub.recordedPaths(), "GET /repos/orkanoio/orkano") {
		t.Fatalf("expected a default-branch lookup, paths = %v", stub.recordedPaths())
	}
	if stub.recordedCommitRef() != "trunk" {
		t.Fatalf("commit resolved at ref %q, want the discovered default branch trunk", stub.recordedCommitRef())
	}

	// The installation lookup used the App JWT; the data calls used the opaque
	// installation token — never the JWT.
	if !looksLikeJWT(stub.recordedInstallationAuth()) {
		t.Fatalf("installation lookup auth = %q, want an App JWT", stub.recordedInstallationAuth())
	}
	if stub.recordedRepoAuth() != "Bearer ghs_test_token" {
		t.Fatalf("default-branch auth = %q, want the installation token", stub.recordedRepoAuth())
	}
	if stub.recordedCommitAuth() != "Bearer ghs_test_token" {
		t.Fatalf("commit auth = %q, want the installation token", stub.recordedCommitAuth())
	}
}

func TestResolveCommitExplicitRefSkipsDefaultLookup(t *testing.T) {
	key := mustKey(t)
	stub := &repoStub{commitSHA: testSHA}
	src := newResolver(t, key, stub)

	if _, err := src.ResolveCommit(context.Background(), testRepo, "release/1.x"); err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	// An explicit ref means no default-branch lookup.
	if contains(stub.recordedPaths(), "GET /repos/orkanoio/orkano") {
		t.Fatalf("explicit ref must not trigger a default-branch lookup, paths = %v", stub.recordedPaths())
	}
	// A slashed branch survives URL escaping as one ref with several segments.
	if stub.recordedCommitRef() != "release/1.x" {
		t.Fatalf("commit ref = %q, want release/1.x (slash preserved)", stub.recordedCommitRef())
	}
}

func TestResolveCommitUnresolvable(t *testing.T) {
	key := mustKey(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		repo string
		ref  string
		stub *repoStub
	}{
		{name: "installation 404", repo: testRepo, ref: "main", stub: &repoStub{installationStatus: http.StatusNotFound}},
		{name: "commit 404", repo: testRepo, ref: "main", stub: &repoStub{commitStatus: http.StatusNotFound}},
		{name: "default-branch 404", repo: testRepo, ref: "", stub: &repoStub{repoStatus: http.StatusNotFound}},
		{name: "commit 422", repo: testRepo, ref: "main", stub: &repoStub{commitStatus: http.StatusUnprocessableEntity}},
		{name: "malformed repo", repo: "no-slash", ref: "main", stub: &repoStub{}},
		{name: "ref with ..", repo: testRepo, ref: "../../app/installations/1", stub: &repoStub{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newResolver(t, key, tc.stub)
			_, err := src.ResolveCommit(ctx, tc.repo, tc.ref)
			if !errors.Is(err, ErrUnresolvable) {
				t.Fatalf("error = %v, want ErrUnresolvable (the dispatcher drops the doorbell)", err)
			}
		})
	}
}

func TestResolveCommitTransientErrorIsRetryable(t *testing.T) {
	key := mustKey(t)
	// A 500 from GitHub is transient: it must NOT be ErrUnresolvable, so the
	// dispatcher leaves the doorbell queued and retries.
	src := newResolver(t, key, &repoStub{commitStatus: http.StatusInternalServerError})
	_, err := src.ResolveCommit(context.Background(), testRepo, "main")
	if err == nil {
		t.Fatal("expected an error for a 500")
	}
	if errors.Is(err, ErrUnresolvable) {
		t.Fatalf("a 500 must be transient, not ErrUnresolvable: %v", err)
	}
}

func TestResolveCommitRejectsNonSHA(t *testing.T) {
	key := mustKey(t)
	// A 200 whose sha is not a 40-char hex string must error rather than pin a
	// bogus image tag downstream (INV-06's first line of defense). It is
	// unresolvable, not transient: a schema change or MITM won't fix on retry,
	// so the dispatcher drops the doorbell instead of wedging the queue.
	src := newResolver(t, key, &repoStub{commitSHA: "not-a-sha"})
	_, err := src.ResolveCommit(context.Background(), testRepo, "main")
	if !errors.Is(err, ErrUnresolvable) {
		t.Fatalf("error = %v, want ErrUnresolvable for a non-SHA commit response", err)
	}
}

func TestSplitRepo(t *testing.T) {
	for _, tc := range []struct {
		in          string
		owner, name string
		ok          bool
	}{
		{in: "orkanoio/orkano", owner: "orkanoio", name: "orkano", ok: true},
		{in: "noslash", ok: false},
		{in: "/name", ok: false},
		{in: "owner/", ok: false},
		{in: "a/b/c", ok: false},
	} {
		owner, name, ok := splitRepo(tc.in)
		if ok != tc.ok || owner != tc.owner || name != tc.name {
			t.Errorf("splitRepo(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.in, owner, name, ok, tc.owner, tc.name, tc.ok)
		}
	}
}

// --- stub accessors (lock-guarded reads) ---

func (s *repoStub) recordedPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}
func (s *repoStub) recordedCommitRef() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitRef
}
func (s *repoStub) recordedInstallationAuth() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.installationAuth
}
func (s *repoStub) recordedCommitAuth() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitAuth
}
func (s *repoStub) recordedRepoAuth() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repoAuth
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func looksLikeJWT(authz string) bool {
	return strings.HasPrefix(authz, "Bearer ") && strings.Count(strings.TrimPrefix(authz, "Bearer "), ".") == 2
}
