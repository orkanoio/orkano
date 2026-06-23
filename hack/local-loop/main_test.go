package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var sha1Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// TestGitFixtureServesCloneAndFetchBySHA is the hermetic E2E's load-bearing
// proof, run on the host instead of in-cluster: that gitfixture/seed.sh seeds a
// bare repo and gitBackendHandler serves it over plain HTTP well enough for
// both a normal clone AND a fetch of the exact commit SHA — the latter is how
// BuildKit fetches a Build's commit-pinned git context (it fetches by SHA, not
// by ref), and is the one operation the whole engine E2E rests on. git on the
// host runs the same smart-HTTP protocol BuildKit's git frontend does, so this
// is a faithful stand-in that does not need a cluster.
func TestGitFixtureServesCloneAndFetchBySHA(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}

	root := t.TempDir()
	sha := seedFixture(t, root, "orkanoio/orkano-e2e")
	if !sha1Re.MatchString(sha) {
		t.Fatalf("seed.sh printed %q, want a 40-hex SHA", sha)
	}

	h, err := gitBackendHandler(root)
	if err != nil {
		t.Fatalf("gitBackendHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	repoURL := srv.URL + "/orkanoio/orkano-e2e.git"

	// A normal clone over plain http (smart HTTP) succeeds and carries the
	// fixture's content — the App-repo clone path.
	clone := t.TempDir()
	gitRun(t, "", "clone", "--quiet", repoURL, clone)
	if got, err := os.ReadFile(filepath.Join(clone, "Dockerfile")); err != nil {
		t.Errorf("cloned repo missing the fixture Dockerfile: %v", err)
	} else if !strings.Contains(string(got), "busybox") {
		t.Errorf("cloned Dockerfile is not the fixture app: %q", got)
	}

	// Fetch the exact commit SHA into a fresh repo — what BuildKit does with a
	// commit-pinned context. seed.sh sets uploadpack.allowAnySHA1InWant, and the
	// commit is also the advertised main tip, so this must succeed.
	fresh := t.TempDir()
	gitRun(t, fresh, "init", "--quiet")
	gitRun(t, fresh, "fetch", "--quiet", repoURL, sha)
}

// seedFixture runs gitfixture/seed.sh against the real fixture working tree and
// returns the HEAD SHA it prints — exercising seed.sh itself, not a reimplementation.
func seedFixture(t *testing.T, root, repo string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "../ci/e2e/gitfixture/seed.sh", "../ci/e2e/fixture", root, repo)
	cmd.Env = hermeticGitEnv()
	// seed.sh prints only the HEAD SHA on stdout (git output is -q or on stderr),
	// so read stdout directly rather than guessing the last token of a merged stream.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("seed.sh: %v\n%s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = hermeticGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// hermeticGitEnv neutralises host/user git config (identity, hooks, http
// protocol restrictions) so the test behaves the same on any machine and in CI.
func hermeticGitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
}
