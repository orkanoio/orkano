package controller

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/operator/internal/buildjob"
)

const (
	// buildFinalizer makes deleting a Build cancel its Job: Builds and Jobs
	// live in different namespaces, so ownerReference GC cannot do it.
	buildFinalizer = "orkano.io/build"

	// jobNameLabel is what the Job controller stamps on the pods it creates;
	// listing by it is how a failed build's pod is found for triage.
	jobNameLabel = "batch.kubernetes.io/job-name"

	reasonPending          = "Pending"
	reasonRunning          = "Running"
	reasonSucceeded        = "Succeeded"
	reasonBuildFailed      = "BuildFailed"
	reasonOOMKilled        = "OOMKilled"
	reasonDeadlineExceeded = "DeadlineExceeded"
	reasonJobConflict      = "JobConflict"
	reasonJobMissing       = "JobMissing"
	reasonResolvingDigest  = "ResolvingDigest"
)

// BuildReconciler runs one Build as one rootless BuildKit Job and mirrors
// the Job's lifecycle into the Build's phase. The build log is deliberately
// nothing more than the Job's pod logs, reachable through status.jobRef.
type BuildReconciler struct {
	client.Client
	// APIReader bypasses the cache for the two reads where staleness would
	// be acted on irreversibly: confirming a tracked Job is really gone
	// before failing the Build, and inspecting pods of an already-failed Job.
	APIReader client.Reader
	// ResolveDigest turns the pushed tag into a digest-pinned reference via
	// a registry manifest HEAD (registry.Resolver in production; tests stub
	// it — envtest has no registry).
	ResolveDigest func(ctx context.Context, imageRef string) (string, error)
}

func (r *BuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var build orkanov1alpha1.Build
	if err := r.Get(ctx, req.NamespacedName, &build); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !build.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &build)
	}
	// A terminal Build is a closed record: spec is immutable, status never
	// changes again, and its Job stays around as the build log until the
	// Build itself is deleted.
	if isTerminal(&build) {
		return ctrl.Result{}, nil
	}

	if controllerutil.AddFinalizer(&build, buildFinalizer) {
		if err := r.Update(ctx, &build); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	statusBefore := build.Status.DeepCopy()
	job, err := r.ensureJob(ctx, &build)
	if err != nil {
		// ensureJob recorded what it learned on the Build (a terminal
		// JobConflict/JobMissing phase, or nothing for transient errors);
		// the status write must land either way. Terminal phases end the
		// retries at the isTerminal gate, transient errors back off.
		return ctrl.Result{}, errors.Join(err, r.updateStatus(ctx, &build, statusBefore))
	}
	observeErr := r.observeJob(ctx, &build, job)
	return ctrl.Result{}, errors.Join(observeErr, r.updateStatus(ctx, &build, statusBefore))
}

// ensureJob creates the Build's Job or adopts the one a previous reconcile
// created before crashing (the annotations are the proof of ownership —
// cross-namespace ownerReferences are forbidden). A same-name Job that is
// not ours is refused, never adopted, never deleted.
func (r *BuildReconciler) ensureJob(ctx context.Context, build *orkanov1alpha1.Build) (*batchv1.Job, error) {
	inv := buildjob.Compose(build)
	desired, err := buildjob.Render(build, buildjob.Options{
		ContextURL:          inv.ContextURL,
		DockerfilePath:      inv.DockerfilePath,
		GeneratedDockerfile: inv.GeneratedDockerfile,
		ImageRef:            inv.ImageRef,
	})
	if err != nil {
		return nil, fmt.Errorf("rendering Job: %w", err)
	}

	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), &job)
	switch {
	case apierrors.IsNotFound(err):
		if build.Status.JobRef != nil {
			// The ref was only ever written after a successful create, so a
			// vanished Job means someone deleted it. Confirm against the
			// live apiserver — the informer may simply not have seen our own
			// create yet — then fail terminally rather than silently rebuild.
			if confirmErr := r.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), &job); confirmErr == nil {
				return &job, nil
			} else if !apierrors.IsNotFound(confirmErr) {
				return nil, fmt.Errorf("confirming Job %s is gone: %w", desired.Name, confirmErr)
			}
			failBuild(build, reasonJobMissing, fmt.Sprintf(
				"Job %s/%s was deleted before the build finished; retry by creating a new Build", desired.Namespace, desired.Name))
			return nil, fmt.Errorf("job %s/%s vanished mid-build", desired.Namespace, desired.Name)
		}
		createErr := r.Create(ctx, desired)
		if apierrors.IsAlreadyExists(createErr) {
			// The cache lagged a Job that already exists. Re-read it live and
			// fall through to the same ownership gate the cached path uses.
			if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), &job); err != nil {
				return nil, fmt.Errorf("fetching existing Job %s after create conflict: %w", desired.Name, err)
			}
			if !ownsJob(build, &job) {
				return nil, r.refuseForeignJob(build, &job)
			}
		} else if createErr != nil {
			return nil, fmt.Errorf("creating Job: %w", createErr)
		} else {
			logf.FromContext(ctx).Info("created build Job", "job", desired.Name)
			job = *desired
		}
	case err != nil:
		return nil, fmt.Errorf("fetching Job %s: %w", desired.Name, err)
	case !ownsJob(build, &job):
		return nil, r.refuseForeignJob(build, &job)
	}

	build.Status.JobRef = &orkanov1alpha1.JobReference{Namespace: buildjob.Namespace, Name: desired.Name}
	return &job, nil
}

// refuseForeignJob is terminal like every other dead end: leaving the phase
// non-terminal would retry forever, show a misleading Pending in kubectl,
// and pin App.status.latestBuild to a build that can never run.
func (r *BuildReconciler) refuseForeignJob(build *orkanov1alpha1.Build, job *batchv1.Job) error {
	failBuild(build, reasonJobConflict, fmt.Sprintf(
		"Job %s/%s already exists and does not belong to this Build; remove it and retry with a new Build", job.Namespace, job.Name))
	return fmt.Errorf("job %s/%s is not owned by Build %s/%s", job.Namespace, job.Name, build.Namespace, build.Name)
}

// observeJob derives phase, timestamps, and the Completed condition from the
// Job, and on completion resolves the pushed digest. Succeeded is only ever
// written together with a digest-pinned status.image: an image-less
// "Succeeded" would advance App rollouts to nothing.
func (r *BuildReconciler) observeJob(ctx context.Context, build *orkanov1alpha1.Build, job *batchv1.Job) error {
	if build.Status.StartedAt == nil && job.Status.StartTime != nil {
		build.Status.StartedAt = job.Status.StartTime
	}

	failedCond := jobCondition(job, batchv1.JobFailed)
	switch {
	case failedCond != nil:
		build.Status.CompletedAt = failedCond.LastTransitionTime.DeepCopy()
		reason, message, err := r.failureReason(ctx, build, job, failedCond)
		if err != nil {
			return err
		}
		failBuild(build, reason, message)

	case jobCondition(job, batchv1.JobComplete) != nil:
		ref := buildjob.Compose(build).ImageRef
		pinned, err := r.ResolveDigest(ctx, ref)
		if err != nil {
			// The image is pushed but unpinned: stay Running, record why,
			// and let the backoff retry — never guess a digest (INV-06).
			setCompleted(build, metav1.ConditionFalse, reasonResolvingDigest,
				fmt.Sprintf("image pushed; resolving its digest failed: %v", err))
			build.Status.Phase = orkanov1alpha1.BuildRunning
			return fmt.Errorf("resolving digest for %s: %w", ref, err)
		}
		build.Status.Image = pinned
		build.Status.Phase = orkanov1alpha1.BuildSucceeded
		build.Status.CompletedAt = job.Status.CompletionTime
		setCompleted(build, metav1.ConditionTrue, reasonSucceeded, "pushed "+pinned)

	case job.Status.Active > 0:
		build.Status.Phase = orkanov1alpha1.BuildRunning
		setCompleted(build, metav1.ConditionFalse, reasonRunning, "build Job is running")

	default:
		// Only ever the first phase: a cache-lagged reconcile may briefly
		// see Active back at 0, and Running must not regress to Pending.
		if build.Status.Phase == "" || build.Status.Phase == orkanov1alpha1.BuildPending {
			build.Status.Phase = orkanov1alpha1.BuildPending
			setCompleted(build, metav1.ConditionFalse, reasonPending, "build Job is waiting to start")
		}
	}
	return nil
}

// failureReason distinguishes the two failure modes a user can act on —
// timeout (raise timeoutSeconds) and OOM (raise build resources) — from
// everything else, which points at the build log.
func (r *BuildReconciler) failureReason(ctx context.Context, build *orkanov1alpha1.Build, job *batchv1.Job, cond *batchv1.JobCondition) (string, string, error) {
	if cond.Reason == batchv1.JobReasonDeadlineExceeded {
		return reasonDeadlineExceeded, fmt.Sprintf("build exceeded its %ds timeout", effectiveTimeout(build)), nil
	}
	// Uncached on purpose: the Job just flipped to Failed and the pod
	// informer may not have caught up with the final container statuses.
	var pods corev1.PodList
	if err := r.APIReader.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{jobNameLabel: job.Name}); err != nil {
		return "", "", fmt.Errorf("listing pods of failed Job %s: %w", job.Name, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		// Init container failures (e.g. the static render-dockerfile step)
		// stop the build before the buildkit container runs, so they surface
		// only in InitContainerStatuses — scan both lists.
		for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
			for _, cs := range statuses {
				term := cs.State.Terminated
				if term == nil {
					term = cs.LastTerminationState.Terminated
				}
				if term == nil {
					continue
				}
				if term.Reason == "OOMKilled" {
					return reasonOOMKilled, "build was OOM-killed; it needs more than the 4Gi memory limit or a smaller build", nil
				}
				if term.ExitCode != 0 {
					return reasonBuildFailed, fmt.Sprintf("build exited with code %d; see the Job pod logs (%s/%s)", term.ExitCode, job.Namespace, job.Name), nil
				}
			}
		}
	}
	message := cond.Message
	if message == "" {
		message = "build Job failed; see the Job pod logs"
	}
	return reasonBuildFailed, message, nil
}

// finalize cancels and cleans up: deleting a Build deletes its Job (and the
// pods holding the build log) — but only a Job the annotations prove ours.
func (r *BuildReconciler) finalize(ctx context.Context, build *orkanov1alpha1.Build) error {
	if !controllerutil.ContainsFinalizer(build, buildFinalizer) {
		return nil
	}
	var job batchv1.Job
	key := types.NamespacedName{Namespace: buildjob.Namespace, Name: buildjob.JobName(build.Name)}
	err := r.Get(ctx, key, &job)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("fetching Job %s for cleanup: %w", key.Name, err)
	case ownsJob(build, &job):
		// Background propagation is explicit because the batch API's legacy
		// default is to orphan pods.
		if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting Job %s: %w", key.Name, err)
		}
		logf.FromContext(ctx).Info("deleted build Job with its Build", "job", key.Name)
	}
	controllerutil.RemoveFinalizer(build, buildFinalizer)
	if err := r.Update(ctx, build); err != nil {
		return fmt.Errorf("removing finalizer: %w", err)
	}
	return nil
}

func (r *BuildReconciler) updateStatus(ctx context.Context, build *orkanov1alpha1.Build, before *orkanov1alpha1.BuildStatus) error {
	build.Status.ObservedGeneration = build.Generation
	if equality.Semantic.DeepEqual(before, &build.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, build); err != nil {
		return fmt.Errorf("updating Build status: %w", err)
	}
	return nil
}

func ownsJob(build *orkanov1alpha1.Build, job *batchv1.Job) bool {
	return job.Annotations[buildjob.AnnotationBuildName] == build.Name &&
		job.Annotations[buildjob.AnnotationBuildNamespace] == build.Namespace
}

func isTerminal(build *orkanov1alpha1.Build) bool {
	return build.Status.Phase == orkanov1alpha1.BuildSucceeded || build.Status.Phase == orkanov1alpha1.BuildFailed
}

func failBuild(build *orkanov1alpha1.Build, reason, message string) {
	build.Status.Phase = orkanov1alpha1.BuildFailed
	setCompleted(build, metav1.ConditionFalse, reason, message)
}

func setCompleted(build *orkanov1alpha1.Build, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
		Type:               orkanov1alpha1.ConditionCompleted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: build.Generation,
	})
}

func effectiveTimeout(build *orkanov1alpha1.Build) int32 {
	if build.Spec.TimeoutSeconds > 0 {
		return build.Spec.TimeoutSeconds
	}
	return buildjob.DefaultTimeoutSeconds
}

func jobCondition(job *batchv1.Job, condType batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range job.Status.Conditions {
		c := &job.Status.Conditions[i]
		if c.Type == condType && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

func (r *BuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orkanov1alpha1.Build{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(mapJobToBuild)).
		Named("build").
		Complete(r)
}

// mapJobToBuild inverts the annotation link Render stamps on every Job; a
// Job without it (foreign, or from another namespace's tests) maps nowhere.
func mapJobToBuild(_ context.Context, obj client.Object) []reconcile.Request {
	ann := obj.GetAnnotations()
	name, namespace := ann[buildjob.AnnotationBuildName], ann[buildjob.AnnotationBuildNamespace]
	if name == "" || namespace == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}
