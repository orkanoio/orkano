package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/doctor"
)

// stubDoctorClient injects a fake cluster client and captures the kubeconfig
// path the command resolved.
func stubDoctorClient(t *testing.T, c ctrlclient.Client, err error) *string {
	t.Helper()
	orig := newDoctorClient
	t.Cleanup(func() { newDoctorClient = orig })
	var gotPath string
	newDoctorClient = func(path string) (ctrlclient.Client, error) {
		gotPath = path
		return c, err
	}
	return &gotPath
}

func doctorScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	return scheme
}

// doctorFakeCluster builds the command tests' cluster: the dashboard Service,
// the registry Service the netpol canaries target, and a Create interceptor
// playing the kubelet — the control canary always connects, the deny canary
// is blocked when netpolEnforced (the healthy case) and connects when not.
func doctorFakeCluster(t *testing.T, svcType corev1.ServiceType, netpolEnforced bool, extra ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	objs := append([]ctrlclient.Object{
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-system", Name: "orkano-dashboard"},
			Spec:       corev1.ServiceSpec{Type: svcType},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-system", Name: "orkano-registry"},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.43.0.7"},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "kubernetes"},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.43.0.1"},
		},
	}, extra...)
	denyPhase := corev1.PodFailed
	if !netpolEnforced {
		denyPhase = corev1.PodSucceeded
	}
	return fake.NewClientBuilder().WithScheme(doctorScheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
				if pod, ok := obj.(*corev1.Pod); ok {
					switch {
					case strings.Contains(pod.Name, "-control-"):
						pod.Status.Phase = corev1.PodSucceeded
					case strings.Contains(pod.Name, "-deny-"):
						pod.Status.Phase = denyPhase
					}
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
}

// healthyClusterCert keeps the healthy-cluster fixtures passing the
// tls.certificate-expiry check (a real install always has the platform PKI).
func healthyClusterCert() ctrlclient.Object {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	u.SetNamespace("orkano-system")
	u.SetName("orkano-registry-tls")
	if err := unstructured.SetNestedMap(u.Object, map[string]interface{}{
		"notAfter":    time.Now().Add(300 * 24 * time.Hour).Format(time.RFC3339),
		"renewalTime": time.Now().Add(270 * 24 * time.Hour).Format(time.RFC3339),
	}, "status"); err != nil {
		panic(err)
	}
	return u
}

func TestDoctorHealthyCluster(t *testing.T) {
	gotPath := stubDoctorClient(t, doctorFakeCluster(t, corev1.ServiceTypeClusterIP, true, healthyClusterCert()), nil)

	var out bytes.Buffer
	err := runDoctor(context.Background(), &out, &doctorOptions{kubeconfig: "custom.kubeconfig"})
	if err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, out.String())
	}
	if *gotPath != "custom.kubeconfig" {
		t.Errorf("client built from %q, want the --kubeconfig value", *gotPath)
	}
	for _, want := range []string{"exposure.dashboard-not-public", "Hardening score: 100%"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestDoctorCriticalFailureExitsOne(t *testing.T) {
	stubDoctorClient(t, doctorFakeCluster(t, corev1.ServiceTypeNodePort, false), nil)

	var out bytes.Buffer
	err := runDoctor(context.Background(), &out, &doctorOptions{})
	if err == nil {
		t.Fatalf("expected a gating error:\n%s", out.String())
	}
	if code := ExitCode(err); code != 1 {
		t.Fatalf("ExitCode = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "[FAIL") {
		t.Errorf("report missing the FAIL line:\n%s", out.String())
	}
}

func TestDoctorJSONOutput(t *testing.T) {
	stubDoctorClient(t, doctorFakeCluster(t, corev1.ServiceTypeNodePort, false), nil)

	var out bytes.Buffer
	err := runDoctor(context.Background(), &out, &doctorOptions{jsonOut: true})
	if err == nil {
		t.Fatal("expected a gating error")
	}
	var rep struct {
		Score    struct{ Value int }
		ExitCode int `json:"exitCode"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("output is not JSON: %v\n%s", jerr, out.String())
	}
	if rep.ExitCode != 1 || rep.Score.Value != 0 {
		t.Errorf("report = %+v, want exitCode 1 and score 0", rep)
	}
}

func TestDoctorLocalRunsNodeChecks(t *testing.T) {
	stubDoctorClient(t, doctorFakeCluster(t, corev1.ServiceTypeClusterIP, true, healthyClusterCert()), nil)
	stubLocalNode(t, func(cmd string) (string, string, int) {
		if cmd == "cat /sys/kernel/security/apparmor/profiles" {
			return "orkano-buildkit (enforce)\n", "", 0
		}
		return "", "unexpected command: " + cmd, 1
	})

	var out bytes.Buffer
	if err := runDoctor(context.Background(), &out, &doctorOptions{local: true}); err != nil {
		t.Fatalf("runDoctor: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "build.apparmor-profile-loaded") {
		t.Errorf("output missing the node check:\n%s", out.String())
	}
}

// TestDoctorIndeterminateExitsTwo drives a probe error (an unreachable
// apiserver) through runDoctor end to end: a critical check that could not be
// determined must gate CI with exit code 2, never 0.
func TestDoctorIndeterminateExitsTwo(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(doctorScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object, ...ctrlclient.GetOption) error {
				return errors.New("apiserver unreachable")
			},
		}).Build()
	stubDoctorClient(t, c, nil)

	var out bytes.Buffer
	err := runDoctor(context.Background(), &out, &doctorOptions{})
	if err == nil {
		t.Fatalf("expected a gating error:\n%s", out.String())
	}
	if code := ExitCode(err); code != 2 {
		t.Fatalf("ExitCode = %d, want 2; err = %v", code, err)
	}
	if !strings.Contains(err.Error(), "could not be determined") {
		t.Errorf("error %q should say the check was indeterminate", err)
	}
	if !strings.Contains(out.String(), "[ERROR") {
		t.Errorf("report missing the ERROR line:\n%s", out.String())
	}
}

// TestDoctorFixFlow proves the --fix flag actually routes through RunAndFix and
// the attempts reach the report: a fixable failing check registered through the
// seam resolves and the resolved line renders.
func TestDoctorFixFlow(t *testing.T) {
	stubDoctorClient(t, doctorFakeCluster(t, corev1.ServiceTypeClusterIP, true, healthyClusterCert()), nil)
	orig := registerDoctorChecks
	t.Cleanup(func() { registerDoctorChecks = orig })
	registerDoctorChecks = func(reg *checks.Registry, opt doctor.Options) error {
		if err := orig(reg, opt); err != nil {
			return err
		}
		broken := true
		return reg.Register(check.Check{
			ID:       "test.toggle",
			Severity: check.SeverityWarning,
			Summary:  "fails until fixed",
			Probe: func(context.Context) (check.Result, error) {
				if broken {
					return check.Result{Status: check.StatusFail, Message: "broken"}, nil
				}
				return check.Result{Status: check.StatusPass, Message: "mended"}, nil
			},
			Fix: func(context.Context) error { broken = false; return nil },
		})
	}

	var out bytes.Buffer
	if err := runDoctor(context.Background(), &out, &doctorOptions{fix: true}); err != nil {
		t.Fatalf("runDoctor --fix: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "fix test.toggle: applied — the check now passes") {
		t.Errorf("output missing the resolved fix line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Hardening score: 100%") {
		t.Errorf("post-fix run should score 100%%:\n%s", out.String())
	}
}

func TestDoctorLocalRefusesNonRoot(t *testing.T) {
	stubDoctorClient(t, doctorFakeCluster(t, corev1.ServiceTypeClusterIP, true), nil)
	orig := geteuid
	t.Cleanup(func() { geteuid = orig })
	geteuid = func() int { return 501 }

	var out bytes.Buffer
	err := runDoctor(context.Background(), &out, &doctorOptions{local: true})
	if err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("expected the root refusal, got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("no report should be written before the refusal:\n%s", out.String())
	}
}

func TestDoctorClientBuildFailure(t *testing.T) {
	stubDoctorClient(t, nil, errors.New("no such kubeconfig"))

	var out bytes.Buffer
	err := runDoctor(context.Background(), &out, &doctorOptions{})
	if err == nil || !strings.Contains(err.Error(), "no such kubeconfig") {
		t.Fatalf("expected the client error, got %v", err)
	}
}

func TestResolveKubeconfig(t *testing.T) {
	for _, tc := range []struct{ flag, env, want string }{
		{"flag.kubeconfig", "env.kubeconfig", "flag.kubeconfig"},
		{"", "env.kubeconfig", "env.kubeconfig"},
		{"", "", "orkano.kubeconfig"},
	} {
		if got := resolveKubeconfig(tc.flag, tc.env); got != tc.want {
			t.Errorf("resolveKubeconfig(%q, %q) = %q, want %q", tc.flag, tc.env, got, tc.want)
		}
	}
}

func TestExitCode(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil) = %d", got)
	}
	if got := ExitCode(errors.New("boom")); got != 1 {
		t.Errorf("ExitCode(generic) = %d", got)
	}
	wrapped := fmt.Errorf("doctor: %w", &exitCodeError{code: 2, msg: "indeterminate"})
	if got := ExitCode(wrapped); got != 2 {
		t.Errorf("ExitCode(wrapped exitCodeError{2}) = %d", got)
	}
}
