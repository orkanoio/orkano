package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
	"github.com/orkanoio/orkano/operator/internal/buildjob"
)

// warningRecorder captures apiserver warning headers, which is how PSA warn
// mode speaks.
type warningRecorder struct {
	mu       sync.Mutex
	warnings []string
}

func (w *warningRecorder) HandleWarningHeaderWithContext(_ context.Context, _ int, _ string, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warnings = append(w.warnings, text)
}

func (w *warningRecorder) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.warnings
	w.warnings = nil
	return out
}

// createPSANamespace labels all three PSA modes at the same level. The
// namespace is never deleted: envtest runs no namespace controller, and a
// dedicated name keeps the labels away from every other test. The template's
// orkano-build ServiceAccount is created alongside — the ServiceAccount
// admission plugin refuses pods whose SA does not exist.
func createPSANamespace(t *testing.T, ctx context.Context, name, level string) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		Labels: map[string]string{
			"pod-security.kubernetes.io/enforce": level,
			"pod-security.kubernetes.io/warn":    level,
			"pod-security.kubernetes.io/audit":   level,
		},
	}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating %s namespace: %v", name, err)
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "orkano-build", Namespace: name}}
	if err := k8sClient.Create(ctx, sa); err != nil {
		t.Fatalf("creating orkano-build ServiceAccount in %s: %v", name, err)
	}
	return name
}

// Half one of the Job-template acceptance: "admitted at baseline with zero
// warnings", capability-probed against the real PodSecurity admission in
// the envtest apiserver. Both detectors are negative-probed at restricted,
// so a silently disabled plugin or a handler that never sees warnings
// cannot produce a vacuous pass. Half two — the rendered Job building a
// public repo end to end — is the substrate smoke's probe 10.
func TestBuildJobAdmittedAtBaselineWithZeroWarnings(t *testing.T) {
	ctx := context.Background()

	rec := &warningRecorder{}
	cfg := rest.CopyConfig(restConfig)
	cfg.WarningHandlerWithContext = rec
	c, err := client.New(cfg, client.Options{Scheme: k8sClient.Scheme()})
	if err != nil {
		t.Fatalf("building warning-capturing client: %v", err)
	}

	render := func(name, generatedDockerfile string) *batchv1.Job {
		t.Helper()
		job, err := buildjob.Render(
			&orkanov1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
				Spec:       orkanov1alpha1.BuildSpec{TimeoutSeconds: 600},
			},
			buildjob.Options{
				ContextURL:          "https://github.com/orkanoio/orkano.git#main",
				GeneratedDockerfile: generatedDockerfile,
				ImageRef:            buildjob.RegistryHost + "/psa/probe:v1",
			},
		)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		return job
	}

	baseline := createPSANamespace(t, ctx, "buildjob-psa-baseline", "baseline")
	restricted := createPSANamespace(t, ctx, "buildjob-psa-restricted", "restricted")

	// Enforce leg. PSA enforcement gates Pods, not workload resources, and
	// envtest runs no Job controller — so admit the pod spec directly, the
	// same object the Job controller would submit.
	job := render("psa-probe", "")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "buildjob-psa-pod", Namespace: baseline, Labels: job.Spec.Template.Labels},
		Spec:       *job.Spec.Template.Spec.DeepCopy(),
	}
	rec.take()
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("build pod rejected at PSA baseline: %v", err)
	}
	if w := rec.take(); len(w) > 0 {
		t.Errorf("build pod admitted at baseline but with warnings: %q", w)
	}

	// Static builds add a render-dockerfile init container; it carries its own
	// securityContext, so it must clear baseline PSA too (the main container's
	// rootless deviations are baseline-only; a restricted-grade init container
	// must not regress that).
	staticJob := render("psa-probe-static", "FROM x\nCOPY public/ /usr/share/nginx/html/\n")
	staticPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "buildjob-psa-static-pod", Namespace: baseline, Labels: staticJob.Spec.Template.Labels},
		Spec:       *staticJob.Spec.Template.Spec.DeepCopy(),
	}
	rec.take()
	if err := c.Create(ctx, staticPod); err != nil {
		t.Fatalf("static build pod (with init container) rejected at PSA baseline: %v", err)
	}
	if w := rec.take(); len(w) > 0 {
		t.Errorf("static build pod admitted at baseline but with warnings: %q", w)
	}
	// Warn mode evaluates the Job's pod template, init containers included.
	staticJob.Namespace = baseline
	if err := c.Create(ctx, staticJob); err != nil {
		t.Fatalf("static build Job rejected in the baseline namespace: %v", err)
	}
	if w := rec.take(); len(w) > 0 {
		t.Errorf("static build Job admitted at baseline but with warnings: %q", w)
	}

	// Warn leg: PSA warn mode evaluates the Job's pod template.
	job.Namespace = baseline
	if err := c.Create(ctx, job); err != nil {
		t.Fatalf("build Job rejected in the baseline namespace: %v", err)
	}
	if w := rec.take(); len(w) > 0 {
		t.Errorf("build Job admitted at baseline but with warnings: %q", w)
	}

	// Detector controls at restricted: enforcement must reject the pod and
	// warn mode must flag the Job, or the asserts above proved nothing.
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "buildjob-psa-pod", Namespace: restricted},
		Spec:       *render("psa-probe", "").Spec.Template.Spec.DeepCopy(),
	}
	err = c.Create(ctx, pod2)
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "PodSecurity") {
		t.Fatalf("PodSecurity enforcement is not active in this apiserver (pod create at restricted: %v) — the baseline admission assert is vacuous", err)
	}
	rec.take()
	job2 := render("psa-probe-restricted", "")
	job2.Namespace = restricted
	if err := c.Create(ctx, job2); err != nil {
		t.Fatalf("creating detector Job at restricted: %v", err)
	}
	warned := false
	for _, w := range rec.take() {
		warned = warned || strings.Contains(w, "PodSecurity")
	}
	if !warned {
		t.Fatal("no PodSecurity warning surfaced for a restricted-violating Job — the zero-warning asserts are vacuous")
	}
}
