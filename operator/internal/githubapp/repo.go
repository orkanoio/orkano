package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ErrUnresolvable marks a repo or ref that will never resolve, so retrying is
// pointless: the App is not installed on the repo, the repo is gone, the ref
// does not exist (404/422), the ref is malformed, or GitHub answered 200 with a
// structurally invalid body. The dispatcher drops such a doorbell rather than
// leave it to head-of-line-block the FIFO queue; every other failure (network,
// 5xx, auth, rate limit, an unparseable body) is transient and retried.
var ErrUnresolvable = errors.New("github: repository or ref cannot be resolved")

var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ResolveCommit returns the full 40-character commit SHA at ref for the repo
// "owner/name". An empty ref means the repo's default branch, fetched from
// GitHub — so a repo whose default is "master" (or anything else) works with no
// configuration.
//
// The webhook payload is never trusted (INV-04): the dispatcher passes only the
// repo and the App's own configured ref, and ResolveCommit reads the
// authoritative HEAD from the API. It looks up the App installation that covers
// the repo (App-JWT auth), mints a short-lived installation token for it, and
// reads the commit with that token. The token is never logged or returned.
func (s *TokenSource) ResolveCommit(ctx context.Context, repo, ref string) (string, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return "", fmt.Errorf("github: repo %q is not in owner/name form: %w", repo, ErrUnresolvable)
	}
	// The ref has no CRD pattern (only a length bound) and it lands in the URL
	// path: a ".." segment would traverse to a different API endpoint. Reject it
	// up front so an obviously-bad ref costs no API calls; the default branch
	// GitHub returns is re-checked the same way in defaultBranch.
	if strings.Contains(ref, "..") {
		return "", fmt.Errorf("github: ref %q contains '..': %w", ref, ErrUnresolvable)
	}

	// installationID re-reads the App Secret to sign an App JWT; InstallationToken
	// reads it again on a cache miss. Two reads per dispatch is fine — they keep
	// the RBAC at one resourceNames-pinned secrets get and make rotation just work.
	installationID, err := s.installationID(ctx, owner, name)
	if err != nil {
		return "", err
	}
	token, err := s.InstallationToken(ctx, installationID)
	if err != nil {
		return "", err
	}

	if ref == "" {
		ref, err = s.defaultBranch(ctx, token, owner, name)
		if err != nil {
			return "", err
		}
	}

	var commit struct {
		SHA string `json:"sha"`
	}
	if err := s.getJSON(ctx, "Bearer "+token, githubPath("repos", owner, name, "commits", ref), &commit); err != nil {
		return "", err
	}
	if !commitSHAPattern.MatchString(commit.SHA) {
		// A 200 with a non-SHA body is a schema change or a MITM, not a transient
		// glitch — retrying never fixes it, so drop rather than wedge the queue.
		return "", fmt.Errorf("github: commit %s/%s@%s resolved to %q, not a 40-char SHA: %w", owner, name, ref, commit.SHA, ErrUnresolvable)
	}
	return commit.SHA, nil
}

// installationID resolves which App installation covers owner/name, using an
// App-level JWT (the only credential that can read this endpoint). A 404 means
// the App is not installed on the repo — unresolvable, so the dispatcher drops
// the doorbell rather than retry forever.
func (s *TokenSource) installationID(ctx context.Context, owner, name string) (int64, error) {
	jwt, err := s.mintAppJWT(ctx)
	if err != nil {
		return 0, err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := s.getJSON(ctx, "Bearer "+jwt, githubPath("repos", owner, name, "installation"), &inst); err != nil {
		return 0, err
	}
	if inst.ID <= 0 {
		return 0, fmt.Errorf("github: installation lookup for %s/%s returned id %d: %w", owner, name, inst.ID, ErrUnresolvable)
	}
	return inst.ID, nil
}

func (s *TokenSource) defaultBranch(ctx context.Context, token, owner, name string) (string, error) {
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := s.getJSON(ctx, "Bearer "+token, githubPath("repos", owner, name), &repoInfo); err != nil {
		return "", err
	}
	if repoInfo.DefaultBranch == "" {
		return "", fmt.Errorf("github: %s/%s response carried no default_branch: %w", owner, name, ErrUnresolvable)
	}
	// Re-apply the ".." guard to the branch GitHub returned before it lands in
	// the commit URL path (git-check-ref-format forbids it, but trust nothing).
	if strings.Contains(repoInfo.DefaultBranch, "..") {
		return "", fmt.Errorf("github: %s/%s default branch %q contains '..': %w", owner, name, repoInfo.DefaultBranch, ErrUnresolvable)
	}
	return repoInfo.DefaultBranch, nil
}

// getJSON performs an authenticated GET and decodes a 200 body into out. A
// 404/422 becomes ErrUnresolvable (a permanent miss the dispatcher drops); every
// other non-200 (and an unparseable 200) is a transient error carrying GitHub's
// (truncated) message.
func (s *TokenSource) getJSON(ctx context.Context, authorization, path string, out any) error {
	endpoint := strings.TrimRight(s.baseURL(), "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("github: building request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)

	resp, err := s.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("github: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("github: reading %s response: %w", path, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("github: decoding %s response: %w", path, err)
		}
		return nil
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		return fmt.Errorf("github: GET %s answered %s: %w", path, resp.Status, ErrUnresolvable)
	default:
		return fmt.Errorf("github: GET %s answered %s: %s", path, resp.Status, truncate(strings.TrimSpace(string(body)), 256))
	}
}

// githubPath joins URL path segments under a leading slash and escapes each so
// spaces, '#', or '?' in a segment cannot alter the request, while a '/' inside
// a multi-segment ref ("release/1.x") is preserved as a separator. owner/name
// are CRD-pattern-validated to [A-Za-z0-9_.-]; escaping them is belt-and-braces,
// but it turns even a malformed repo into a clean 404 (drop) instead of a
// request-build error (retry forever).
func githubPath(segments ...string) string {
	return (&url.URL{Path: "/" + strings.Join(segments, "/")}).EscapedPath()
}

// splitRepo splits "owner/name" on its single slash; anything else (no slash,
// empty half, an extra slash) is rejected.
func splitRepo(repo string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return owner, name, true
}
