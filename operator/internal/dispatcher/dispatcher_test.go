package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/operator/internal/githubapp"
)

// These tests drive the dispatcher's orchestration over a controller-runtime
// fake client. Build admissibility (the snapshot is a valid Build per CEL) is
// proven against a real apiserver by examples_test.go; here the focus is the
// dispatch logic: app resolution, the snapshot, idempotency, drop-vs-retry, and
// the concurrency cap.

const (
	testNS   = "orkano-apps"
	testSHA  = "abcdef0123456789abcdef0123456789abcdef01" // 40 hex
	testSHA2 = "1111111111111111111111111111111111111111"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := orkanov1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func makeApp(name, repo, ref string) *orkanov1alpha1.App {
	return &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: orkanov1alpha1.AppSpec{
			Source: orkanov1alpha1.Source{GitHub: orkanov1alpha1.GitHubSource{Repo: repo, Ref: ref}},
			Build:  orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
		},
	}
}

func newDispatcher(c client.Client, q Queue, g CommitResolver) *Dispatcher {
	return &Dispatcher{
		Client:              c,
		Queue:               q,
		GitHub:              g,
		Log:                 logr.Discard(),
		MaxConcurrentBuilds: 10,
	}
}

func listBuilds(t *testing.T, c client.Client) []orkanov1alpha1.Build {
	t.Helper()
	var builds orkanov1alpha1.BuildList
	if err := c.List(context.Background(), &builds); err != nil {
		t.Fatalf("list builds: %v", err)
	}
	return builds.Items
}

// --- fakes ---

type fakeGitHub struct {
	// Keyed on "repo@ref" (lowercased repo), matching what calls records — so a
	// monorepo's two Apps tracking different refs can have distinct outcomes.
	sha map[string]string
	err map[string]error

	mu    sync.Mutex
	calls []string
}

func ghKey(repo, ref string) string { return strings.ToLower(repo) + "@" + ref }

func (g *fakeGitHub) ResolveCommit(_ context.Context, repo, ref string) (string, error) {
	g.mu.Lock()
	g.calls = append(g.calls, repo+"@"+ref)
	g.mu.Unlock()
	key := ghKey(repo, ref)
	if err := g.err[key]; err != nil {
		return "", err
	}
	if sha, ok := g.sha[key]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("fakeGitHub: no sha configured for %s", key)
}

type fakeRow struct {
	id                          int64
	deliveryID, repo, eventType string
}

type fakeQueue struct {
	mu      sync.Mutex
	pending []fakeRow
	acked   []int64
	nacked  []int64
}

func (q *fakeQueue) enqueue(rows ...fakeRow) { q.pending = append(q.pending, rows...) }

func (q *fakeQueue) ClaimNext(_ context.Context) (*Delivery, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil, nil
	}
	row := q.pending[0]
	q.pending = q.pending[1:]
	return &Delivery{
		ID: row.id, DeliveryID: row.deliveryID, Repo: row.repo, EventType: row.eventType,
		ack: func(context.Context) error {
			q.mu.Lock()
			defer q.mu.Unlock()
			q.acked = append(q.acked, row.id)
			return nil
		},
		nack: func(context.Context) error {
			q.mu.Lock()
			defer q.mu.Unlock()
			q.nacked = append(q.nacked, row.id)
			// The row stays for a later poll: put it back at the front.
			q.pending = append([]fakeRow{row}, q.pending...)
			return nil
		},
	}, nil
}

// --- tests ---

func TestDispatcherCreatesBuildFromApp(t *testing.T) {
	app := makeApp("web", "orkanoio/web", "")
	app.Spec.Source.SubPath = "services/api"
	app.Spec.Build = orkanov1alpha1.BuildStrategy{
		Strategy:   orkanov1alpha1.StrategyDockerfile,
		Dockerfile: &orkanov1alpha1.DockerfileBuild{Path: "Dockerfile.prod"},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()
	q := &fakeQueue{}
	q.enqueue(fakeRow{id: 1, deliveryID: "d1", repo: "orkanoio/web", eventType: "push"})
	d := newDispatcher(c, q, &fakeGitHub{sha: map[string]string{"orkanoio/web@": testSHA}})

	d.tick(context.Background())

	if len(q.acked) != 1 || q.acked[0] != 1 {
		t.Fatalf("acked = %v, want [1] (processed delivery removed)", q.acked)
	}
	if len(q.nacked) != 0 {
		t.Fatalf("nacked = %v, want none", q.nacked)
	}

	builds := listBuilds(t, c)
	if len(builds) != 1 {
		t.Fatalf("created %d Builds, want 1", len(builds))
	}
	b := builds[0]
	if b.Name != buildName("web", testSHA) {
		t.Errorf("Build name = %q, want %q", b.Name, buildName("web", testSHA))
	}
	if b.Namespace != testNS {
		t.Errorf("Build namespace = %q, want %q", b.Namespace, testNS)
	}
	if b.Labels[managedByLabel] != managedByValue {
		t.Errorf("managed-by label = %q, want %q", b.Labels[managedByLabel], managedByValue)
	}
	// The snapshot: full 40-char SHA + source/strategy copied verbatim.
	if b.Spec.AppName != "web" {
		t.Errorf("AppName = %q, want web", b.Spec.AppName)
	}
	if b.Spec.Commit != testSHA {
		t.Errorf("Commit = %q, want the full 40-char SHA %q", b.Spec.Commit, testSHA)
	}
	if b.Spec.Source.SubPath != "services/api" {
		t.Errorf("Source.SubPath = %q, want services/api (snapshot)", b.Spec.Source.SubPath)
	}
	if b.Spec.Strategy.Strategy != orkanov1alpha1.StrategyDockerfile ||
		b.Spec.Strategy.Dockerfile == nil || b.Spec.Strategy.Dockerfile.Path != "Dockerfile.prod" {
		t.Errorf("Strategy = %+v, want the App's Dockerfile strategy snapshotted", b.Spec.Strategy)
	}
}

func TestDispatcherMonorepoFanOut(t *testing.T) {
	// Two Apps back the same repo via distinct subPaths: one doorbell must build
	// both.
	api := makeApp("api", "orkanoio/mono", "")
	api.Spec.Source.SubPath = "api"
	web := makeApp("web", "orkanoio/mono", "")
	web.Spec.Source.SubPath = "web"
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(api, web).Build()
	q := &fakeQueue{}
	q.enqueue(fakeRow{id: 1, deliveryID: "d1", repo: "orkanoio/mono", eventType: "push"})
	d := newDispatcher(c, q, &fakeGitHub{sha: map[string]string{"orkanoio/mono@": testSHA}})

	d.tick(context.Background())

	if len(q.acked) != 1 {
		t.Fatalf("acked = %v, want one ack for the single doorbell", q.acked)
	}
	builds := listBuilds(t, c)
	if len(builds) != 2 {
		t.Fatalf("created %d Builds, want 2 (one per App sharing the repo)", len(builds))
	}
	gotApps := map[string]bool{}
	for _, b := range builds {
		gotApps[b.Spec.AppName] = true
	}
	if !gotApps["api"] || !gotApps["web"] {
		t.Fatalf("Builds cover apps %v, want both api and web", gotApps)
	}
}

func TestDispatcherDuplicateDeliveryIsIdempotent(t *testing.T) {
	app := makeApp("web", "orkanoio/web", "")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()
	q := &fakeQueue{}
	// Two distinct deliveries for the same repo — both resolve to the same HEAD.
	q.enqueue(
		fakeRow{id: 1, deliveryID: "d1", repo: "orkanoio/web", eventType: "push"},
		fakeRow{id: 2, deliveryID: "d2", repo: "orkanoio/web", eventType: "push"},
	)
	d := newDispatcher(c, q, &fakeGitHub{sha: map[string]string{"orkanoio/web@": testSHA}})

	d.tick(context.Background())

	if len(q.acked) != 2 {
		t.Fatalf("acked = %v, want both deliveries removed", q.acked)
	}
	if builds := listBuilds(t, c); len(builds) != 1 {
		t.Fatalf("created %d Builds, want 1 (the same commit collapses to one Build)", len(builds))
	}
}

func TestDispatcherNoAppDropsDelivery(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build() // no Apps
	q := &fakeQueue{}
	q.enqueue(fakeRow{id: 1, deliveryID: "d1", repo: "orkanoio/orphan", eventType: "push"})
	d := newDispatcher(c, q, &fakeGitHub{})

	d.tick(context.Background())

	if len(q.acked) != 1 {
		t.Fatalf("acked = %v, want the orphan doorbell dropped (acked)", q.acked)
	}
	if len(q.nacked) != 0 {
		t.Fatalf("nacked = %v, want none (a no-App doorbell is permanent, not retried)", q.nacked)
	}
	if builds := listBuilds(t, c); len(builds) != 0 {
		t.Fatalf("created %d Builds, want 0", len(builds))
	}
}

func TestDispatcherUnresolvableDropsDelivery(t *testing.T) {
	app := makeApp("web", "orkanoio/web", "deleted-branch")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()
	q := &fakeQueue{}
	q.enqueue(fakeRow{id: 1, deliveryID: "d1", repo: "orkanoio/web", eventType: "push"})
	d := newDispatcher(c, q, &fakeGitHub{
		err: map[string]error{"orkanoio/web@deleted-branch": fmt.Errorf("ref gone: %w", githubapp.ErrUnresolvable)},
	})

	d.tick(context.Background())

	if len(q.acked) != 1 || len(q.nacked) != 0 {
		t.Fatalf("acked=%v nacked=%v, want the unresolvable doorbell dropped (acked)", q.acked, q.nacked)
	}
	if builds := listBuilds(t, c); len(builds) != 0 {
		t.Fatalf("created %d Builds, want 0", len(builds))
	}
}

func TestDispatcherTransientErrorRetries(t *testing.T) {
	app := makeApp("web", "orkanoio/web", "")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()
	q := &fakeQueue{}
	q.enqueue(fakeRow{id: 1, deliveryID: "d1", repo: "orkanoio/web", eventType: "push"})
	d := newDispatcher(c, q, &fakeGitHub{
		err: map[string]error{"orkanoio/web@": errors.New("github 503")},
	})

	d.tick(context.Background())

	if len(q.nacked) != 1 {
		t.Fatalf("nacked = %v, want the doorbell left for retry on a transient error", q.nacked)
	}
	if len(q.acked) != 0 {
		t.Fatalf("acked = %v, want none (a transient failure must not remove the row)", q.acked)
	}
	if len(q.pending) != 1 {
		t.Fatalf("pending = %d, want 1 (a nacked row must be re-queued for the next poll)", len(q.pending))
	}
	if builds := listBuilds(t, c); len(builds) != 0 {
		t.Fatalf("created %d Builds, want 0", len(builds))
	}
}

func TestDispatcherRespectsConcurrencyCap(t *testing.T) {
	app := makeApp("web", "orkanoio/web", "")
	// One Build already in flight (non-terminal) and a cap of 1: the budget is 0,
	// so the tick returns before polling the queue at all — ClaimNext, Ack, and
	// Nack are never called.
	running := &orkanov1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "in-flight", Namespace: testNS},
		Status:     orkanov1alpha1.BuildStatus{Phase: orkanov1alpha1.BuildRunning},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app, running).Build()
	q := &fakeQueue{}
	q.enqueue(fakeRow{id: 1, deliveryID: "d1", repo: "orkanoio/web", eventType: "push"})
	d := newDispatcher(c, q, &fakeGitHub{sha: map[string]string{"orkanoio/web@": testSHA}})
	d.MaxConcurrentBuilds = 1

	d.tick(context.Background())

	if len(q.acked) != 0 || len(q.nacked) != 0 {
		t.Fatalf("acked=%v nacked=%v, want neither: the queue must not be polled when the budget is 0", q.acked, q.nacked)
	}
	if len(q.pending) != 1 {
		t.Fatalf("pending = %d, want the delivery still queued", len(q.pending))
	}
	if builds := listBuilds(t, c); len(builds) != 1 {
		t.Fatalf("Builds = %d, want only the pre-existing in-flight one (no new Build at the cap)", len(builds))
	}

	// Free capacity: the in-flight Build completes, and the next tick proceeds.
	running.Status.Phase = orkanov1alpha1.BuildSucceeded
	if err := c.Update(context.Background(), running); err != nil {
		t.Fatalf("update build: %v", err)
	}
	d.tick(context.Background())
	if len(q.acked) != 1 {
		t.Fatalf("acked = %v, want the delivery processed once capacity freed", q.acked)
	}
	if builds := listBuilds(t, c); len(builds) != 2 {
		t.Fatalf("Builds = %d, want 2 (the new Build created once under the cap)", len(builds))
	}
}

func TestDispatcherMatchesRepoCaseInsensitively(t *testing.T) {
	// The App carries the user's casing; the stored doorbell carries the
	// payload's. GitHub repos are case-insensitive, so they must still match.
	app := makeApp("web", "Orkanoio/Web", "")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()
	q := &fakeQueue{}
	q.enqueue(fakeRow{id: 1, deliveryID: "d1", repo: "orkanoio/web", eventType: "push"})
	d := newDispatcher(c, q, &fakeGitHub{sha: map[string]string{"orkanoio/web@": testSHA}})

	d.tick(context.Background())

	if builds := listBuilds(t, c); len(builds) != 1 {
		t.Fatalf("created %d Builds, want 1 (case-insensitive repo match)", len(builds))
	}
}

func TestDispatcherForwardsRefToGitHub(t *testing.T) {
	// The App's configured ref must be the one re-resolved (deterministic commit
	// pinning), not silently replaced with the default branch.
	app := makeApp("web", "orkanoio/web", "release/v1")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()
	q := &fakeQueue{}
	q.enqueue(fakeRow{id: 1, deliveryID: "d1", repo: "orkanoio/web", eventType: "push"})
	g := &fakeGitHub{sha: map[string]string{"orkanoio/web@release/v1": testSHA}}
	d := newDispatcher(c, q, g)

	d.tick(context.Background())

	if len(g.calls) != 1 || g.calls[0] != "orkanoio/web@release/v1" {
		t.Fatalf("ResolveCommit calls = %v, want exactly [orkanoio/web@release/v1]", g.calls)
	}
	if builds := listBuilds(t, c); len(builds) != 1 || builds[0].Spec.Commit != testSHA {
		t.Fatalf("builds = %+v, want one Build pinned to %s", builds, testSHA)
	}
}

func TestDispatcherPartialMonorepoBuildsResolvable(t *testing.T) {
	// Two Apps share the repo on different refs: one ref is gone (unresolvable),
	// the other resolves. The resolvable App must still get a Build, and the
	// doorbell is dropped (acked) — not nacked, not blocked.
	api := makeApp("api", "orkanoio/mono", "main")
	api.Spec.Source.SubPath = "api"
	web := makeApp("web", "orkanoio/mono", "deleted-branch")
	web.Spec.Source.SubPath = "web"
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(api, web).Build()
	q := &fakeQueue{}
	q.enqueue(fakeRow{id: 1, deliveryID: "d1", repo: "orkanoio/mono", eventType: "push"})
	d := newDispatcher(c, q, &fakeGitHub{
		sha: map[string]string{"orkanoio/mono@main": testSHA},
		err: map[string]error{"orkanoio/mono@deleted-branch": fmt.Errorf("ref gone: %w", githubapp.ErrUnresolvable)},
	})

	d.tick(context.Background())

	if len(q.acked) != 1 || len(q.nacked) != 0 {
		t.Fatalf("acked=%v nacked=%v, want the doorbell dropped after building the resolvable App", q.acked, q.nacked)
	}
	builds := listBuilds(t, c)
	if len(builds) != 1 {
		t.Fatalf("created %d Builds, want 1 (only the resolvable App)", len(builds))
	}
	if builds[0].Spec.AppName != "api" {
		t.Fatalf("Build is for App %q, want api (the resolvable one)", builds[0].Spec.AppName)
	}
}

func TestBuildName(t *testing.T) {
	// Deterministic and readable for normal names.
	if got := buildName("web", testSHA); got != "web-"+testSHA[:buildNameSHALen] {
		t.Errorf("buildName(web) = %q, want web-%s", got, testSHA[:buildNameSHALen])
	}
	// Same inputs -> same name (the idempotency seed); different commit -> different.
	if first, second := buildName("web", testSHA), buildName("web", testSHA); first != second {
		t.Errorf("buildName must be deterministic: %q != %q", first, second)
	}
	if buildName("web", testSHA) == buildName("web", testSHA2) {
		t.Error("different commits must yield different Build names")
	}
	// A pathologically long App name still yields a valid, capped, unique name.
	long := strings.Repeat("a", 300)
	got := buildName(long, testSHA)
	if len(got) > maxNameLen {
		t.Fatalf("buildName length = %d, want <= %d", len(got), maxNameLen)
	}
	if got == buildName(strings.Repeat("b", 300), testSHA) {
		t.Error("distinct long App names must not collide after trimming (the hash tail disambiguates)")
	}
	if strings.HasSuffix(got, "-") || strings.Contains(got, "--") {
		t.Errorf("buildName(long) = %q, not a clean DNS-1123 name", got)
	}
}
