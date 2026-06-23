package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/operator/internal/buildjob"
	"github.com/orkanoio/orkano/operator/internal/registry"
)

// stubDigest lets each test script the digest resolution the manager-wired
// BuildReconciler performs; the default answers deterministically so the
// suite's other Builds (App status, examples) never depend on ordering.
var stubDigest struct {
	mu sync.Mutex
	fn func(ctx context.Context, imageRef string) (string, error)
}

func stubResolveDigest(ctx context.Context, imageRef string) (string, error) {
	stubDigest.mu.Lock()
	fn := stubDigest.fn
	stubDigest.mu.Unlock()
	if fn != nil {
		return fn(ctx, imageRef)
	}
	return fakeDigestPin(imageRef), nil
}

func setDigestStub(t *testing.T, fn func(ctx context.Context, imageRef string) (string, error)) {
	t.Helper()
	stubDigest.mu.Lock()
	stubDigest.fn = fn
	stubDigest.mu.Unlock()
	t.Cleanup(func() {
		stubDigest.mu.Lock()
		stubDigest.fn = nil
		stubDigest.mu.Unlock()
	})
}

func fakeDigestPin(imageRef string) string {
	repo := imageRef
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	sum := sha256.Sum256([]byte(imageRef))
	return repo + "@sha256:" + hex.EncodeToString(sum[:])
}

func buildJobKey(buildName string) types.NamespacedName {
	return types.NamespacedName{Namespace: buildjob.Namespace, Name: buildjob.JobName(buildName)}
}

func waitForBuild(t *testing.T, name, desc string, cond func(*orkanov1alpha1.Build) bool) *orkanov1alpha1.Build {
	t.Helper()
	var build orkanov1alpha1.Build
	eventually(t, "Build "+name+" "+desc, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appsNamespace}, &build); err != nil {
			return false, err
		}
		return cond(&build), nil
	})
	return &build
}

func waitForCompleted(t *testing.T, name, reason string) *orkanov1alpha1.Build {
	t.Helper()
	return waitForBuild(t, name, "Completed reason "+reason, func(b *orkanov1alpha1.Build) bool {
		cond := meta.FindStatusCondition(b.Status.Conditions, orkanov1alpha1.ConditionCompleted)
		return cond != nil && cond.Reason == reason
	})
}

// setBuildJobStatus plays the Job controller, which envtest does not run.
func setBuildJobStatus(t *testing.T, buildName string, mutate func(*batchv1.Job)) {
	t.Helper()
	key := buildJobKey(buildName)
	eventually(t, "Job status update for "+key.Name, func(ctx context.Context) (bool, error) {
		var job batchv1.Job
		if err := k8sClient.Get(ctx, key, &job); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil // the controller has not created it yet
			}
			return false, err
		}
		mutate(&job)
		if err := k8sClient.Status().Update(ctx, &job); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

func markJobFailed(t *testing.T, buildName, reason, message string) {
	t.Helper()
	setBuildJobStatus(t, buildName, func(job *batchv1.Job) {
		now := metav1.Now()
		if job.Status.StartTime == nil {
			job.Status.StartTime = &now
		}
		job.Status.Conditions = append(job.Status.Conditions,
			batchv1.JobCondition{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: reason, Message: message, LastTransitionTime: now},
			batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: message, LastTransitionTime: now})
	})
}

func markJobComplete(t *testing.T, buildName string) {
	t.Helper()
	setBuildJobStatus(t, buildName, func(job *batchv1.Job) {
		now := metav1.Now()
		if job.Status.StartTime == nil {
			// Never overwrite: startTime is immutable once set for an
			// unsuspended Job, and the stored value only equals a fresh
			// metav1.Now() when both writes land in the same second.
			job.Status.StartTime = &now
		}
		job.Status.CompletionTime = &now
		job.Status.Conditions = append(job.Status.Conditions,
			batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
			batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now})
	})
}

// createJobPod plays the pod the Job controller would create, so failure
// triage has container statuses to inspect.
func createJobPod(t *testing.T, buildName string, terminated corev1.ContainerStateTerminated) {
	t.Helper()
	ctx := context.Background()
	key := buildJobKey(buildName)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name + "-pod",
			Namespace: key.Namespace,
			Labels:    map[string]string{jobNameLabel: key.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "buildkit", Image: "stub"}},
		},
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("creating Job pod: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pod) })
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "buildkit",
			State: corev1.ContainerState{Terminated: &terminated},
		}},
	}
	if err := k8sClient.Status().Update(ctx, pod); err != nil {
		t.Fatalf("setting Job pod status: %v", err)
	}
}

func TestBuildCreatesJobAndReportsPending(t *testing.T) {
	ctx := context.Background()
	build := createBuild(t, "bc-pending", "bc-pending-app")

	got := waitForBuild(t, build.Name, "jobRef + Pending", func(b *orkanov1alpha1.Build) bool {
		return b.Status.JobRef != nil && b.Status.Phase == orkanov1alpha1.BuildPending
	})
	if got.Status.JobRef.Namespace != buildjob.Namespace || got.Status.JobRef.Name != buildjob.JobName(build.Name) {
		t.Errorf("jobRef = %+v, want %s/%s", got.Status.JobRef, buildjob.Namespace, buildjob.JobName(build.Name))
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, orkanov1alpha1.ConditionCompleted)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reasonPending {
		t.Errorf("Completed condition = %+v, want False/%s", cond, reasonPending)
	}

	var job batchv1.Job
	if err := k8sClient.Get(ctx, buildJobKey(build.Name), &job); err != nil {
		t.Fatalf("fetching created Job: %v", err)
	}
	if job.Annotations[buildjob.AnnotationBuildName] != build.Name || job.Annotations[buildjob.AnnotationBuildNamespace] != appsNamespace {
		t.Errorf("Job annotations %v do not map back to the Build", job.Annotations)
	}
	if sa := job.Spec.Template.Spec.ServiceAccountName; sa != "orkano-build" {
		t.Errorf("Job pod serviceAccountName = %q, want orkano-build", sa)
	}
	if got := *job.Spec.ActiveDeadlineSeconds; got != buildjob.DefaultTimeoutSeconds {
		t.Errorf("activeDeadlineSeconds = %d, want the spec default %d", got, buildjob.DefaultTimeoutSeconds)
	}
	// The base is the suite reconciler's configured --git-base-url sentinel, so
	// this also proves r.GitBaseURL threads through Compose into the Job context.
	wantContext := "http://git.example.test/orkanoio/example.git#" + build.Spec.Commit
	if args := job.Spec.Template.Spec.Containers[0].Args; !strings.Contains(strings.Join(args, " "), "--opt=context="+wantContext) {
		t.Errorf("Job args %v miss the commit-pinned context %s", args, wantContext)
	}

	fresh := waitForBuild(t, build.Name, "finalizer", func(b *orkanov1alpha1.Build) bool {
		return len(b.Finalizers) > 0
	})
	if fresh.Finalizers[0] != buildFinalizer {
		t.Errorf("finalizers = %v, want %s", fresh.Finalizers, buildFinalizer)
	}
}

func TestBuildRunsThenSucceedsWithPinnedDigest(t *testing.T) {
	var (
		mu     sync.Mutex
		gotRef string
	)
	setDigestStub(t, func(_ context.Context, imageRef string) (string, error) {
		mu.Lock()
		gotRef = imageRef
		mu.Unlock()
		return fakeDigestPin(imageRef), nil
	})
	build := createBuild(t, "bc-succeed", "bc-succeed-app")

	setBuildJobStatus(t, build.Name, func(job *batchv1.Job) {
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Active = 1
	})
	running := waitForBuild(t, build.Name, "Running", func(b *orkanov1alpha1.Build) bool {
		return b.Status.Phase == orkanov1alpha1.BuildRunning
	})
	if running.Status.StartedAt == nil {
		t.Error("Running build has no startedAt")
	}

	setBuildJobStatus(t, build.Name, func(job *batchv1.Job) { job.Status.Active = 0 })
	markJobComplete(t, build.Name)
	wantRef := buildjob.RegistryHost + "/bc-succeed-app:" + build.Spec.Commit
	done := waitForBuild(t, build.Name, "Succeeded", func(b *orkanov1alpha1.Build) bool {
		return b.Status.Phase == orkanov1alpha1.BuildSucceeded
	})
	mu.Lock()
	resolved := gotRef
	mu.Unlock()
	if resolved != wantRef {
		t.Errorf("digest resolved for %q, want %q", resolved, wantRef)
	}
	if done.Status.Image != fakeDigestPin(wantRef) {
		t.Errorf("status.image = %q, want %q", done.Status.Image, fakeDigestPin(wantRef))
	}
	if !strings.Contains(done.Status.Image, "@sha256:") {
		t.Errorf("status.image %q is not digest-pinned", done.Status.Image)
	}
	if done.Status.CompletedAt == nil || done.Status.StartedAt == nil {
		t.Errorf("terminal build misses timestamps: startedAt=%v completedAt=%v", done.Status.StartedAt, done.Status.CompletedAt)
	}
	cond := meta.FindStatusCondition(done.Status.Conditions, orkanov1alpha1.ConditionCompleted)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reasonSucceeded {
		t.Errorf("Completed condition = %+v, want True/%s", cond, reasonSucceeded)
	}
}

func TestBuildFailureReasonsAreDistinct(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		build := createBuild(t, "bc-deadline", "bc-deadline-app")
		waitForBuild(t, build.Name, "jobRef", func(b *orkanov1alpha1.Build) bool { return b.Status.JobRef != nil })
		markJobFailed(t, build.Name, "DeadlineExceeded", "Job was active longer than specified deadline")
		got := waitForCompleted(t, build.Name, reasonDeadlineExceeded)
		if got.Status.Phase != orkanov1alpha1.BuildFailed {
			t.Errorf("phase = %s, want Failed", got.Status.Phase)
		}
		if !strings.Contains(meta.FindStatusCondition(got.Status.Conditions, orkanov1alpha1.ConditionCompleted).Message,
			fmt.Sprintf("%ds", buildjob.DefaultTimeoutSeconds)) {
			t.Error("deadline message does not name the timeout")
		}
		if got.Status.CompletedAt == nil {
			t.Error("failed build has no completedAt")
		}
	})

	t.Run("oomkilled", func(t *testing.T) {
		build := createBuild(t, "bc-oom", "bc-oom-app")
		waitForBuild(t, build.Name, "jobRef", func(b *orkanov1alpha1.Build) bool { return b.Status.JobRef != nil })
		createJobPod(t, build.Name, corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"})
		markJobFailed(t, build.Name, "BackoffLimitExceeded", "Job has reached the specified backoff limit")
		got := waitForCompleted(t, build.Name, reasonOOMKilled)
		if got.Status.Phase != orkanov1alpha1.BuildFailed {
			t.Errorf("phase = %s, want Failed", got.Status.Phase)
		}
	})

	t.Run("generic", func(t *testing.T) {
		build := createBuild(t, "bc-exit2", "bc-exit2-app")
		waitForBuild(t, build.Name, "jobRef", func(b *orkanov1alpha1.Build) bool { return b.Status.JobRef != nil })
		createJobPod(t, build.Name, corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"})
		markJobFailed(t, build.Name, "BackoffLimitExceeded", "Job has reached the specified backoff limit")
		got := waitForCompleted(t, build.Name, reasonBuildFailed)
		cond := meta.FindStatusCondition(got.Status.Conditions, orkanov1alpha1.ConditionCompleted)
		if !strings.Contains(cond.Message, "code 2") {
			t.Errorf("generic failure message %q does not carry the exit code", cond.Message)
		}
	})
}

func TestBuildDigestFailureStaysRunningThenHeals(t *testing.T) {
	setDigestStub(t, func(context.Context, string) (string, error) {
		return "", fmt.Errorf("registry answered 503")
	})
	build := createBuild(t, "bc-digest-retry", "bc-digest-retry-app")
	waitForBuild(t, build.Name, "jobRef", func(b *orkanov1alpha1.Build) bool { return b.Status.JobRef != nil })
	markJobComplete(t, build.Name)

	got := waitForCompleted(t, build.Name, reasonResolvingDigest)
	if got.Status.Phase != orkanov1alpha1.BuildRunning {
		t.Errorf("phase = %s while digest unresolved, want Running", got.Status.Phase)
	}
	if got.Status.Image != "" {
		t.Errorf("status.image = %q while digest unresolved, want empty", got.Status.Image)
	}

	setDigestStub(t, func(_ context.Context, imageRef string) (string, error) {
		return fakeDigestPin(imageRef), nil
	})
	waitForBuild(t, build.Name, "Succeeded after digest heals", func(b *orkanov1alpha1.Build) bool {
		return b.Status.Phase == orkanov1alpha1.BuildSucceeded && b.Status.Image != ""
	})
}

func TestBuildDeletionCancelsItsJob(t *testing.T) {
	ctx := context.Background()
	build := createBuild(t, "bc-cancel", "bc-cancel-app")
	waitForBuild(t, build.Name, "jobRef", func(b *orkanov1alpha1.Build) bool { return b.Status.JobRef != nil })

	if err := k8sClient.Delete(ctx, build); err != nil {
		t.Fatalf("deleting Build: %v", err)
	}
	eventually(t, "Job to be deleted with its Build", func(ctx context.Context) (bool, error) {
		var job batchv1.Job
		err := k8sClient.Get(ctx, buildJobKey(build.Name), &job)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		// envtest runs no GC, so the Job vanishes only via the finalizer's
		// explicit delete; a deletionTimestamp also counts (background
		// propagation may park it briefly).
		return err == nil && !job.DeletionTimestamp.IsZero(), err
	})
	eventually(t, "Build to be released by its finalizer", func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: build.Name, Namespace: appsNamespace}, &orkanov1alpha1.Build{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
}

func TestBuildRefusesForeignJobName(t *testing.T) {
	ctx := context.Background()
	foreign := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "bc-foreign", Namespace: buildjob.Namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "other", Image: "stub"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, foreign); err != nil {
		t.Fatalf("creating foreign Job: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, foreign) })

	build := createBuild(t, "bc-foreign", "bc-foreign-app")
	got := waitForCompleted(t, build.Name, reasonJobConflict)
	if got.Status.JobRef != nil {
		t.Errorf("Build adopted a foreign Job: jobRef = %+v", got.Status.JobRef)
	}
	if got.Status.Phase != orkanov1alpha1.BuildFailed {
		t.Errorf("phase = %s after JobConflict, want Failed (non-terminal would retry forever)", got.Status.Phase)
	}

	// Deleting the Build must leave the foreign Job alone.
	if err := k8sClient.Delete(ctx, build); err != nil {
		t.Fatalf("deleting Build: %v", err)
	}
	eventually(t, "Build to be gone", func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: build.Name, Namespace: appsNamespace}, &orkanov1alpha1.Build{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
	var job batchv1.Job
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "bc-foreign", Namespace: buildjob.Namespace}, &job); err != nil {
		t.Fatalf("foreign Job was touched by the Build's cleanup: %v", err)
	}
	if !job.DeletionTimestamp.IsZero() {
		t.Error("foreign Job was deleted by the Build's cleanup")
	}
}

func TestBuildFailsWhenItsJobVanishes(t *testing.T) {
	ctx := context.Background()
	build := createBuild(t, "bc-vanish", "bc-vanish-app")
	waitForBuild(t, build.Name, "jobRef", func(b *orkanov1alpha1.Build) bool { return b.Status.JobRef != nil })

	var job batchv1.Job
	if err := k8sClient.Get(ctx, buildJobKey(build.Name), &job); err != nil {
		t.Fatalf("fetching Job: %v", err)
	}
	// Background, not the batch default: orphan propagation would park the
	// Job behind a GC finalizer envtest never processes, and the Job would
	// never actually vanish.
	if err := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatalf("deleting Job out from under the Build: %v", err)
	}
	got := waitForCompleted(t, build.Name, reasonJobMissing)
	if got.Status.Phase != orkanov1alpha1.BuildFailed {
		t.Errorf("phase = %s after Job vanished, want Failed", got.Status.Phase)
	}
}

func TestTerminalBuildIsInert(t *testing.T) {
	ctx := context.Background()
	build := createBuild(t, "bc-inert", "bc-inert-app")
	waitForBuild(t, build.Name, "jobRef", func(b *orkanov1alpha1.Build) bool { return b.Status.JobRef != nil })
	markJobComplete(t, build.Name)
	done := waitForBuild(t, build.Name, "Succeeded", func(b *orkanov1alpha1.Build) bool {
		return b.Status.Phase == orkanov1alpha1.BuildSucceeded
	})

	var job batchv1.Job
	if err := k8sClient.Get(ctx, buildJobKey(build.Name), &job); err != nil {
		t.Fatalf("fetching Job: %v", err)
	}
	// Background for the same reason as in TestBuildFailsWhenItsJobVanishes.
	if err := k8sClient.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatalf("deleting the finished Job: %v", err)
	}
	// Positive barrier first — the deletion is visible — so the pause below
	// starts after the watch event existed to be mishandled.
	eventually(t, "the finished Job to be gone", func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, buildJobKey(build.Name), &batchv1.Job{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
	time.Sleep(1500 * time.Millisecond)
	var fresh orkanov1alpha1.Build
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: build.Name, Namespace: appsNamespace}, &fresh); err != nil {
		t.Fatalf("refetching Build: %v", err)
	}
	// Any reaction at all would bump the resourceVersion; a regression here
	// means the controller resurrects Jobs for closed records.
	if fresh.ResourceVersion != done.ResourceVersion {
		t.Errorf("terminal Build was written after its Job was deleted: %+v", fresh.Status)
	}
	if fresh.Status.Phase != orkanov1alpha1.BuildSucceeded || fresh.Status.Image != done.Status.Image {
		t.Errorf("terminal Build changed after its Job was deleted: %+v", fresh.Status)
	}
	err := k8sClient.Get(ctx, buildJobKey(build.Name), &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Job was recreated for a terminal Build (get err: %v)", err)
	}
}

// The DeepEqual guard in updateStatus is what keeps the controller's own
// status writes from re-triggering it forever; resourceVersion churn on an
// idle Pending build would prove it broken.
func TestBuildPendingNoSpuriousStatusWrite(t *testing.T) {
	ctx := context.Background()
	build := createBuild(t, "bc-no-churn", "bc-no-churn-app")
	got := waitForBuild(t, build.Name, "Pending + jobRef + finalizer", func(b *orkanov1alpha1.Build) bool {
		return b.Status.JobRef != nil && b.Status.Phase == orkanov1alpha1.BuildPending && len(b.Finalizers) > 0
	})
	rv := got.ResourceVersion
	time.Sleep(1500 * time.Millisecond)
	var fresh orkanov1alpha1.Build
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: build.Name, Namespace: appsNamespace}, &fresh); err != nil {
		t.Fatalf("refetching Build: %v", err)
	}
	if fresh.ResourceVersion != rv {
		t.Errorf("Build resourceVersion churned %s → %s with no input change", rv, fresh.ResourceVersion)
	}
}

// TestResolverAgainstTLSRegistry exercises the production Resolver — real
// HEAD requests, TLS verified against a CA fetched from the real apiserver —
// with a local TLS server standing in for the registry.
func TestResolverAgainstTLSRegistry(t *testing.T) {
	ctx := context.Background()
	const digest = "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		switch r.URL.Path {
		case "/v2/smoke/app/manifests/good":
			w.Header().Set("Docker-Content-Digest", digest)
		case "/v2/smoke/app/manifests/headerless":
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	host := strings.TrimPrefix(ts.URL, "https://")

	resolver := &registry.Resolver{Reader: k8sClient}

	if _, err := resolver.ResolveDigest(ctx, host+"/smoke/app:good"); err == nil {
		t.Error("ResolveDigest succeeded without the CA bundle ConfigMap, want error")
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: buildjob.CAConfigMapName, Namespace: buildjob.Namespace},
		Data:       map[string]string{"ca.crt": string(caPEM)},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("creating CA ConfigMap: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, cm) })

	got, err := resolver.ResolveDigest(ctx, host+"/smoke/app:good")
	if err != nil {
		t.Fatalf("ResolveDigest: %v", err)
	}
	if want := host + "/smoke/app@" + digest; got != want {
		t.Errorf("ResolveDigest = %q, want %q", got, want)
	}

	if _, err := resolver.ResolveDigest(ctx, host+"/smoke/app:missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("ResolveDigest for a missing tag = %v, want a 404 error", err)
	}
	if _, err := resolver.ResolveDigest(ctx, host+"/smoke/app:headerless"); err == nil || !strings.Contains(err.Error(), "Docker-Content-Digest") {
		t.Errorf("ResolveDigest without the digest header = %v, want a header error", err)
	}

	// A bundle that does not sign the server's certificate must fail closed.
	// A second httptest server would not do: every httptest server in a
	// process presents the same built-in certificate.
	cm.Data["ca.crt"] = string(selfSignedPEM(t))
	if err := k8sClient.Update(ctx, cm); err != nil {
		t.Fatalf("swapping the CA bundle: %v", err)
	}
	if _, err := resolver.ResolveDigest(ctx, host+"/smoke/app:good"); err == nil {
		t.Error("ResolveDigest trusted a certificate outside the CA bundle")
	}
}

// selfSignedPEM mints a certificate unrelated to any server in this process.
func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "not-the-registry"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
