package cmd

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/orkanoio/orkano/internal/platformsecrets"
)

// stubBootstrapTokenClient injects a fake cluster and captures the kubeconfig
// path the command resolved (the stubDoctorClient idiom).
func stubBootstrapTokenClient(t *testing.T, cs kubernetes.Interface, err error) *string {
	t.Helper()
	orig := newBootstrapTokenClient
	t.Cleanup(func() { newBootstrapTokenClient = orig })
	var gotPath string
	newBootstrapTokenClient = func(path string) (kubernetes.Interface, error) {
		gotPath = path
		return cs, err
	}
	// The rollout poll must not sleep the suite; the fixtures are already Ready.
	origInterval, origTimeout := rolloutPollInterval, rolloutWaitTimeout
	t.Cleanup(func() { rolloutPollInterval, rolloutWaitTimeout = origInterval, origTimeout })
	rolloutPollInterval = time.Millisecond
	rolloutWaitTimeout = 20 * time.Millisecond
	return &gotPath
}

// readyDashboard is a rolled-out dashboard Deployment: the wait returns on its
// first poll.
func readyDashboard() *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       dashboardDeployment,
			Namespace:  platformsecrets.Namespace,
			Generation: 1,
		},
		Spec: appsv1.DeploymentSpec{Replicas: &one},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
		},
	}
}

var tokenLineRe = regexp.MustCompile(`Bootstrap token \(shown once; store it now\):\n  (\S+)\n`)

func tokenFromOutput(t *testing.T, out string) string {
	t.Helper()
	m := tokenLineRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("output carries no bootstrap token:\n%s", out)
	}
	return m[1]
}

func storedHash(t *testing.T, cs kubernetes.Interface) corev1.Secret {
	t.Helper()
	sec, err := cs.CoreV1().Secrets(platformsecrets.Namespace).Get(
		context.Background(), platformsecrets.NameBootstrapToken, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the bootstrap-token Secret: %v", err)
	}
	return *sec
}

func TestBootstrapTokenFirstMint(t *testing.T) {
	cs := fake.NewClientset(readyDashboard())
	stubBootstrapTokenClient(t, cs, nil)

	var out bytes.Buffer
	if err := runBootstrapToken(context.Background(), &out, &bootstrapTokenOptions{}); err != nil {
		t.Fatalf("runBootstrapToken: %v\n%s", err, out.String())
	}

	token := tokenFromOutput(t, out.String())
	if got := string(storedHash(t, cs).Data[platformsecrets.KeyTokenSHA256]); got != platformsecrets.HashToken(token) {
		t.Errorf("stored hash %q is not the sha256 of the printed token", got)
	}
	// ADR-0003: shown once. A second occurrence would make the "store it now"
	// contract a lie and widen the shoulder-surfing window.
	if n := strings.Count(out.String(), token); n != 1 {
		t.Errorf("token appears %d times in the output, want exactly 1:\n%s", n, out.String())
	}

	dep, err := cs.AppsV1().Deployments(platformsecrets.Namespace).Get(context.Background(), dashboardDeployment, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the dashboard Deployment: %v", err)
	}
	if dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] == "" {
		t.Error("the dashboard was not restarted; it would keep comparing against the previous hash")
	}
}

func TestBootstrapTokenRotates(t *testing.T) {
	const oldHash = "0000000000000000000000000000000000000000000000000000000000000000"
	cs := fake.NewClientset(readyDashboard(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: platformsecrets.NameBootstrapToken, Namespace: platformsecrets.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			platformsecrets.KeyTokenSHA256: []byte(oldHash),
			"future-key":                   []byte("keep"),
		},
	})
	stubBootstrapTokenClient(t, cs, nil)

	var out bytes.Buffer
	if err := runBootstrapToken(context.Background(), &out, &bootstrapTokenOptions{}); err != nil {
		t.Fatalf("runBootstrapToken: %v\n%s", err, out.String())
	}

	token := tokenFromOutput(t, out.String())
	sec := storedHash(t, cs)
	if got := string(sec.Data[platformsecrets.KeyTokenSHA256]); got == oldHash {
		t.Error("the previous hash survived; the rotation did not land")
	} else if got != platformsecrets.HashToken(token) {
		t.Errorf("stored hash %q is not the sha256 of the printed token", got)
	}
	// Setting the one key rather than replacing the map: a key a future release
	// adds to this Secret must survive a rotation.
	if string(sec.Data["future-key"]) != "keep" {
		t.Error("rotation destroyed an unrelated key in the bootstrap-token Secret")
	}
}

func TestBootstrapTokenNoDashboard(t *testing.T) {
	cs := fake.NewClientset()
	stubBootstrapTokenClient(t, cs, nil)

	var out bytes.Buffer
	if err := runBootstrapToken(context.Background(), &out, &bootstrapTokenOptions{}); err != nil {
		t.Fatalf("a missing dashboard must not fail the mint: %v\n%s", err, out.String())
	}
	tokenFromOutput(t, out.String())
	if !strings.Contains(out.String(), "will read the new hash when it first starts") {
		t.Errorf("output should explain that a not-yet-deployed dashboard picks the hash up on start:\n%s", out.String())
	}
}

func TestBootstrapTokenRestartFailureStillPrintsToken(t *testing.T) {
	cs := fake.NewClientset(readyDashboard())
	cs.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver said no")
	})
	stubBootstrapTokenClient(t, cs, nil)

	var out bytes.Buffer
	// Exit 0: the hash IS stored, so a non-zero exit would invite the user to
	// discard a credential that already works.
	if err := runBootstrapToken(context.Background(), &out, &bootstrapTokenOptions{}); err != nil {
		t.Fatalf("a failed restart must not fail the mint: %v\n%s", err, out.String())
	}
	tokenFromOutput(t, out.String())
	if !strings.Contains(out.String(), "kubectl -n orkano-system rollout restart deploy/orkano-dashboard") {
		t.Errorf("output should hand over the manual restart command:\n%s", out.String())
	}
}

func TestBootstrapTokenRolloutNotReadyStillPrintsToken(t *testing.T) {
	dep := readyDashboard()
	dep.Generation = 2 // the restarted pods have not been observed yet
	cs := fake.NewClientset(dep)
	stubBootstrapTokenClient(t, cs, nil)

	var out bytes.Buffer
	if err := runBootstrapToken(context.Background(), &out, &bootstrapTokenOptions{}); err != nil {
		t.Fatalf("a slow rollout must not fail the mint: %v\n%s", err, out.String())
	}
	tokenFromOutput(t, out.String())
	if !strings.Contains(out.String(), "rejected until the restarted pod is Ready") {
		t.Errorf("output should warn that the new token is not live yet:\n%s", out.String())
	}
}

// The dashboard is a single replica under the default RollingUpdate strategy,
// so maxSurge rounds to 1 and maxUnavailable to 0: right after the restart the
// new pod exists (updated=1) while the only AVAILABLE pod is still the old one
// serving the old hash. Reporting "ready" there is the exact rotate-again loop
// the wait exists to prevent.
func TestBootstrapTokenSurgingRolloutIsNotReady(t *testing.T) {
	dep := readyDashboard()
	dep.Status.Replicas = 2 // the old pod is still up alongside the new one
	cs := fake.NewClientset(dep)
	stubBootstrapTokenClient(t, cs, nil)

	var out bytes.Buffer
	if err := runBootstrapToken(context.Background(), &out, &bootstrapTokenOptions{}); err != nil {
		t.Fatalf("runBootstrapToken: %v\n%s", err, out.String())
	}
	tokenFromOutput(t, out.String())
	if !strings.Contains(out.String(), "rejected until the restarted pod is Ready") {
		t.Errorf("a surging rollout must not be reported ready; the old pod still holds the old hash:\n%s", out.String())
	}
}

func TestBootstrapTokenSecretWriteFailurePrintsNothing(t *testing.T) {
	cs := fake.NewClientset(readyDashboard())
	cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, platformsecrets.NameBootstrapToken, nil)
	})
	stubBootstrapTokenClient(t, cs, nil)

	var out bytes.Buffer
	err := runBootstrapToken(context.Background(), &out, &bootstrapTokenOptions{})
	if err == nil {
		t.Fatal("expected the Secret write failure to surface")
	}
	// The ordering guarantee: a token whose hash was never stored is unredeemable,
	// so it must never be printed.
	if strings.Contains(out.String(), "Bootstrap token") {
		t.Errorf("a token was printed even though its hash was not stored:\n%s", out.String())
	}
}

func TestBootstrapTokenThreadsKubeconfig(t *testing.T) {
	cs := fake.NewClientset(readyDashboard())
	gotPath := stubBootstrapTokenClient(t, cs, nil)

	var out bytes.Buffer
	if err := runBootstrapToken(context.Background(), &out, &bootstrapTokenOptions{kubeconfig: "custom.kubeconfig"}); err != nil {
		t.Fatalf("runBootstrapToken: %v", err)
	}
	if *gotPath != "custom.kubeconfig" {
		t.Errorf("client built from %q, want the --kubeconfig value", *gotPath)
	}
}

func TestBootstrapTokenClientBuildFailure(t *testing.T) {
	stubBootstrapTokenClient(t, nil, errors.New("no such kubeconfig"))

	var out bytes.Buffer
	if err := runBootstrapToken(context.Background(), &out, &bootstrapTokenOptions{}); err == nil {
		t.Fatal("expected the client build failure to surface")
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be printed when the cluster is unreachable:\n%s", out.String())
	}
}
