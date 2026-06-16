// Package dispatcher consumes the webhook delivery queue (the receiver's
// doorbells) and turns each into immutable Build CRs. A doorbell carries only a
// repo (never the pushed commit — the payload is not trusted, INV-04), so the
// dispatcher re-fetches the authoritative HEAD of each matching App's tracked
// ref from the GitHub API and snapshots the App's source/strategy into a Build
// pinned to that 40-char SHA. The App controller then rolls the workload.
//
// It runs as a leader-only manager Runnable, polling on an interval
// (SELECT … FOR UPDATE SKIP LOCKED — boring beats LISTEN/NOTIFY) and capping the
// number of in-flight Builds it will create.
package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/operator/internal/githubapp"
)

const (
	// DefaultPollInterval is how often the queue is polled when none is set.
	DefaultPollInterval = 5 * time.Second

	// DefaultMaxConcurrentBuilds caps in-flight Builds when none is set.
	DefaultMaxConcurrentBuilds = 5

	// managedByLabel/Value mark Builds the dispatcher created, distinguishing
	// them from Builds applied by hand (the manual-redeploy button).
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "orkano"

	// maxNameLen is the Kubernetes object-name limit; buildName caps at it.
	maxNameLen = 253
	// buildNameSHALen is how much of the commit goes in the Build name — enough
	// to stay unique per App across commits while keeping the name readable.
	buildNameSHALen = 12
)

// CommitResolver re-fetches the authoritative HEAD commit for a repo + ref.
// Satisfied by *githubapp.TokenSource; faked in tests. A ResolveCommit error
// that wraps githubapp.ErrUnresolvable means the repo/ref will never resolve, so
// the dispatcher drops that work rather than retry forever.
type CommitResolver interface {
	ResolveCommit(ctx context.Context, repo, ref string) (sha string, err error)
}

// Dispatcher polls the delivery queue and creates Builds. The zero value is not
// usable: Client, Queue, and GitHub are required.
type Dispatcher struct {
	// Client reads Apps and Builds (cached) and creates Builds.
	Client client.Client
	// Queue is the delivery source (PgxQueue in production).
	Queue Queue
	// GitHub resolves a repo+ref to its current HEAD commit.
	GitHub CommitResolver
	// Log is the dispatcher's logger; the zero Logger is a safe no-op.
	Log logr.Logger
	// PollInterval is the queue poll cadence; <=0 means DefaultPollInterval.
	PollInterval time.Duration
	// MaxConcurrentBuilds caps in-flight (non-terminal) Builds; <=0 means
	// DefaultMaxConcurrentBuilds.
	MaxConcurrentBuilds int
}

// Start runs the poll loop until the context is cancelled. It satisfies
// manager.Runnable.
func (d *Dispatcher) Start(ctx context.Context) error {
	if d.PollInterval <= 0 {
		d.PollInterval = DefaultPollInterval
	}
	if d.MaxConcurrentBuilds <= 0 {
		d.MaxConcurrentBuilds = DefaultMaxConcurrentBuilds
	}
	d.Log.Info("dispatcher started", "pollInterval", d.PollInterval, "maxConcurrentBuilds", d.MaxConcurrentBuilds)

	ticker := time.NewTicker(d.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.Log.Info("dispatcher stopping")
			return nil
		case <-ticker.C:
			d.tick(ctx)
		}
	}
}

// NeedLeaderElection keeps the dispatcher to the elected leader: exactly one
// consumer drains the queue, even with multiple operator replicas.
func (d *Dispatcher) NeedLeaderElection() bool { return true }

// tick drains up to the current build budget of deliveries. It stops early on
// an empty queue or any transient error, backing off to the next poll.
func (d *Dispatcher) tick(ctx context.Context) {
	budget, err := d.budget(ctx)
	if err != nil {
		d.Log.Error(err, "computing build budget; skipping this tick")
		return
	}
	for range budget {
		processed, err := d.processOne(ctx)
		if err != nil {
			d.Log.Error(err, "processing delivery; retrying on the next poll")
			return
		}
		if !processed {
			return
		}
	}
}

// budget is how many more deliveries this tick may process before reaching the
// in-flight Build cap. It is approximate at delivery granularity: a doorbell for
// a monorepo with N Apps creates N Builds, so the cap can be briefly exceeded by
// the fan-out of one delivery.
func (d *Dispatcher) budget(ctx context.Context) (int, error) {
	// Cluster-wide List against the cached client: it returns whatever the cache
	// holds, which M1.5 scopes to the orkano namespaces (Builds live in
	// orkano-apps) before the operator runs under its namespaced RBAC.
	var builds orkanov1alpha1.BuildList
	if err := d.Client.List(ctx, &builds); err != nil {
		return 0, fmt.Errorf("listing Builds for the concurrency cap: %w", err)
	}
	inflight := 0
	for i := range builds.Items {
		if !buildTerminal(&builds.Items[i]) {
			inflight++
		}
	}
	if budget := d.MaxConcurrentBuilds - inflight; budget > 0 {
		return budget, nil
	}
	return 0, nil
}

// processOne claims one delivery and finalizes it: Ack (remove) on success or a
// permanent drop, Nack (leave for retry) on a transient failure. It returns
// (true, nil) when a delivery was removed, (false, nil) when the queue is empty,
// and (false, err) on a transient failure that should end the tick.
func (d *Dispatcher) processOne(ctx context.Context) (bool, error) {
	delivery, err := d.Queue.ClaimNext(ctx)
	if err != nil {
		return false, fmt.Errorf("claiming delivery: %w", err)
	}
	if delivery == nil {
		return false, nil
	}

	ack, handleErr := d.handle(ctx, delivery)
	if !ack {
		if nackErr := delivery.Nack(ctx); nackErr != nil {
			return false, errors.Join(handleErr, fmt.Errorf("nacking delivery %s: %w", delivery.DeliveryID, nackErr))
		}
		return false, handleErr
	}
	if handleErr != nil {
		d.Log.Info("dropping delivery", "delivery", delivery.DeliveryID, "repo", delivery.Repo, "reason", handleErr.Error())
	}
	if ackErr := delivery.Ack(ctx); ackErr != nil {
		return false, fmt.Errorf("acking delivery %s: %w", delivery.DeliveryID, ackErr)
	}
	return true, nil
}

// handle turns one doorbell into Builds. It returns ack=true when the row should
// be removed — every matched App got a Build, or the doorbell is permanently
// unprocessable (no App, or every App's ref is unresolvable) — and ack=false
// when a transient failure means the row should be retried. err carries the
// reason for logging.
func (d *Dispatcher) handle(ctx context.Context, delivery *Delivery) (ack bool, err error) {
	apps, err := d.appsForRepo(ctx, delivery.Repo)
	if err != nil {
		return false, fmt.Errorf("listing Apps for repo %s: %w", delivery.Repo, err)
	}
	if len(apps) == 0 {
		// The receiver allowlist gates which repos enqueue, but an allowlisted
		// repo with no App is a config gap, not work to retry: drop the doorbell.
		return true, fmt.Errorf("no App references repo %s", delivery.Repo)
	}

	unresolvable := 0
	for i := range apps {
		app := &apps[i]
		sha, resolveErr := d.GitHub.ResolveCommit(ctx, app.Spec.Source.GitHub.Repo, app.Spec.Source.GitHub.Ref)
		switch {
		case errors.Is(resolveErr, githubapp.ErrUnresolvable):
			// This App's repo/ref will never resolve. Skip it without failing the
			// doorbell — other Apps sharing the repo (a monorepo) may resolve.
			d.Log.Info("skipping App: repo or ref unresolvable",
				"app", app.Name, "namespace", app.Namespace, "reason", resolveErr.Error())
			unresolvable++
			continue
		case resolveErr != nil:
			return false, fmt.Errorf("resolving commit for App %s/%s: %w", app.Namespace, app.Name, resolveErr)
		}
		if err := d.createBuild(ctx, app, sha); err != nil {
			return false, fmt.Errorf("creating Build for App %s/%s: %w", app.Namespace, app.Name, err)
		}
	}
	if unresolvable == len(apps) {
		// Nothing could be built and nothing will resolve later: drop the doorbell.
		return true, fmt.Errorf("every App for repo %s was unresolvable", delivery.Repo)
	}
	return true, nil
}

// appsForRepo returns every App whose source repo matches, case-insensitively
// (GitHub repos are case-insensitive; the stored doorbell carries the payload's
// canonical case while the App carries the user's). A monorepo can back several
// Apps via distinct subPaths, so the match is one-to-many.
func (d *Dispatcher) appsForRepo(ctx context.Context, repo string) ([]orkanov1alpha1.App, error) {
	// Cluster-wide List against the cached client (see budget): the cache holds
	// the orkano-apps Apps the M1.5-scoped informer watches.
	var apps orkanov1alpha1.AppList
	if err := d.Client.List(ctx, &apps); err != nil {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSpace(repo))
	var matched []orkanov1alpha1.App
	for i := range apps.Items {
		if strings.ToLower(apps.Items[i].Spec.Source.GitHub.Repo) == want {
			matched = append(matched, apps.Items[i])
		}
	}
	return matched, nil
}

// createBuild creates the snapshot Build, treating AlreadyExists as success: the
// deterministic name makes a duplicate delivery (or a re-resolve to the same
// HEAD, or a re-processed row) converge on the one Build for that commit.
func (d *Dispatcher) createBuild(ctx context.Context, app *orkanov1alpha1.App, sha string) error {
	build := buildFromApp(app, sha)
	err := d.Client.Create(ctx, build)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return err
	}
	d.Log.Info("created Build", "build", build.Name, "namespace", build.Namespace, "app", app.Name, "commit", sha)
	return nil
}

// buildFromApp snapshots the App's source and strategy into an immutable Build
// pinned to the resolved commit. TimeoutSeconds is left unset so the CRD default
// applies. The Build is a standalone record (no ownerRef): the App controller
// finds it via the spec.appName index, and its lifecycle is independent of the
// App, matching how Builds are created elsewhere.
func buildFromApp(app *orkanov1alpha1.App, sha string) *orkanov1alpha1.Build {
	return &orkanov1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:      buildName(app.Name, sha),
			Namespace: app.Namespace,
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
		Spec: orkanov1alpha1.BuildSpec{
			AppName:  app.Name,
			Commit:   sha,
			Source:   app.Spec.Source,
			Strategy: app.Spec.Build,
		},
	}
}

// buildName derives a deterministic Build name from the App name and commit, so
// repeated deliveries for the same commit converge on one Build (idempotency).
// It stays within the object-name limit, hashing the App name when it is long
// (mirroring buildjob.JobName). The App name is a DNS-1123 subdomain and the
// commit is lowercase hex, so the result is a valid object name.
func buildName(appName, sha string) string {
	short := sha
	if len(short) > buildNameSHALen {
		short = short[:buildNameSHALen]
	}
	if name := appName + "-" + short; len(name) <= maxNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(appName))
	hash := hex.EncodeToString(sum[:4]) // 8 chars
	prefix := appName
	if max := maxNameLen - len(short) - len(hash) - 2; len(prefix) > max { // 2 dashes
		prefix = prefix[:max]
	}
	return strings.TrimRight(prefix, "-.") + "-" + hash + "-" + short
}

func buildTerminal(b *orkanov1alpha1.Build) bool {
	return b.Status.Phase == orkanov1alpha1.BuildSucceeded || b.Status.Phase == orkanov1alpha1.BuildFailed
}
