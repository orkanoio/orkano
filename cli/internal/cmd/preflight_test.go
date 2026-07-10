package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// stubPreflightCluster injects a fake cluster + server version and captures the
// kubeconfig path the command resolved (the stubDoctorClient idiom).
func stubPreflightCluster(t *testing.T, c ctrlclient.Client, sv func() (*version.Info, error), err error) *string {
	t.Helper()
	orig := newPreflightCluster
	t.Cleanup(func() { newPreflightCluster = orig })
	var gotPath string
	newPreflightCluster = func(path string) (ctrlclient.Client, func() (*version.Info, error), error) {
		gotPath = path
		return c, sv, err
	}
	return &gotPath
}

func serverVersion(major, minor string) func() (*version.Info, error) {
	return func() (*version.Info, error) {
		return &version.Info{Major: major, Minor: minor}, nil
	}
}

// preflightFakeCluster builds a BYO cluster the whole check set passes on:
// a default StorageClass, an IngressClass, one Ready Linux node, an SSAR
// interceptor answering every access review allowed, and a pod interceptor
// playing the kubelet for the live capability probes (statuses stamped on
// Create — the doctor-harness idiom, since the fake client runs no kubelet).
func preflightFakeCluster(t *testing.T, defaultStorage bool) ctrlclient.Client {
	t.Helper()
	storageAnnotations := map[string]string{}
	if defaultStorage {
		storageAnnotations["storageclass.kubernetes.io/is-default-class"] = "true"
	}
	objs := []ctrlclient.Object{
		&storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "standard", Annotations: storageAnnotations},
			Provisioner: "example.com/prov",
		},
		&networkingv1.IngressClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "traefik",
				Annotations: map[string]string{"ingressclass.kubernetes.io/is-default-class": "true"},
			},
			Spec: networkingv1.IngressClassSpec{Controller: "traefik.io/ingress-controller"},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "worker-0",
				Labels: map[string]string{"kubernetes.io/os": "linux"},
			},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}}},
		},
	}
	return fake.NewClientBuilder().WithScheme(doctorScheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
				if ssar, ok := obj.(*authorizationv1.SelfSubjectAccessReview); ok {
					ssar.Status.Allowed = true
					return nil
				}
				if pod, ok := obj.(*corev1.Pod); ok {
					if err := healthyPreflightPod(pod); err != nil {
						return err
					}
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
}

// healthyPreflightPod stamps the terminal status a healthy cluster's kubelet
// would produce for each live-probe canary role (mirrors the cluster package's
// own healthyLivePod fixture — keep in sync when a new live probe lands).
func healthyPreflightPod(pod *corev1.Pod) error {
	terminal := func(exitCode int32) {
		if exitCode == 0 {
			pod.Status.Phase = corev1.PodSucceeded
		} else {
			pod.Status.Phase = corev1.PodFailed
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "probe",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}},
		}}
	}
	switch pod.Labels["orkano.io/preflight-role"] {
	case "netpol-server":
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = "10.244.0.9"
	case "netpol-control":
		terminal(0)
	case "netpol-deny":
		if strings.HasPrefix(pod.GenerateName, "baseline-deny-") {
			terminal(0)
		} else {
			terminal(42)
		}
	case "apparmor", "seccomp":
		terminal(0)
	case "psa":
		return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod.Name,
			errors.New(`violates PodSecurity "restricted:latest"`))
	}
	return nil
}

func TestPreflightHealthyCluster(t *testing.T) {
	gotPath := stubPreflightCluster(t, preflightFakeCluster(t, true), serverVersion("1", "36"), nil)

	var out bytes.Buffer
	err := runClusterPreflight(context.Background(), &out, &preflightOptions{kubeconfig: "byo.kubeconfig"})
	if err != nil {
		t.Fatalf("runClusterPreflight: %v\n%s", err, out.String())
	}
	if *gotPath != "byo.kubeconfig" {
		t.Errorf("cluster built from %q, want the --kubeconfig value", *gotPath)
	}
	for _, want := range []string{
		"cluster.version-supported",
		"cluster.rbac-sufficient",
		"net.networkpolicy-enforced",
		"build.apparmor-capable",
		"8 checks: 8 passed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestPreflightFailureExitsOne(t *testing.T) {
	stubPreflightCluster(t, preflightFakeCluster(t, false), serverVersion("1", "36"), nil)

	var out bytes.Buffer
	err := runClusterPreflight(context.Background(), &out, &preflightOptions{})
	if err == nil {
		t.Fatalf("expected a gating error:\n%s", out.String())
	}
	if code := ExitCode(err); code != 1 {
		t.Fatalf("ExitCode = %d, want 1; err = %v", code, err)
	}
	if !strings.Contains(err.Error(), "do not install") {
		t.Errorf("error %q should warn against installing", err)
	}
	if !strings.Contains(out.String(), "[FAIL") {
		t.Errorf("report missing the FAIL line:\n%s", out.String())
	}
}

// TestPreflightIndeterminateExitsTwo pins the exit-code contract's third leg: a
// check that could not be determined (the server version is unreadable) must
// gate with 2, never pass as 0.
func TestPreflightIndeterminateExitsTwo(t *testing.T) {
	stubPreflightCluster(t, preflightFakeCluster(t, true), func() (*version.Info, error) {
		return nil, errors.New("discovery unreachable")
	}, nil)

	var out bytes.Buffer
	err := runClusterPreflight(context.Background(), &out, &preflightOptions{})
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

func TestPreflightJSONOutput(t *testing.T) {
	stubPreflightCluster(t, preflightFakeCluster(t, false), serverVersion("1", "36"), nil)

	var out bytes.Buffer
	err := runClusterPreflight(context.Background(), &out, &preflightOptions{jsonOut: true})
	if err == nil {
		t.Fatal("expected a gating error")
	}
	var rep struct {
		Results []struct {
			ID string `json:"id"`
		}
		ExitCode int `json:"exitCode"`
	}
	if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
		t.Fatalf("output is not JSON: %v\n%s", jerr, out.String())
	}
	if rep.ExitCode != 1 || len(rep.Results) != 8 {
		t.Errorf("report = exitCode %d with %d results, want exitCode 1 and 8 results", rep.ExitCode, len(rep.Results))
	}
}

func TestPreflightClusterBuildFailure(t *testing.T) {
	stubPreflightCluster(t, nil, nil, errors.New("no such kubeconfig"))

	var out bytes.Buffer
	err := runClusterPreflight(context.Background(), &out, &preflightOptions{})
	if err == nil || !strings.Contains(err.Error(), "no such kubeconfig") {
		t.Fatalf("expected the cluster build error, got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("no report should be written on a client failure:\n%s", out.String())
	}
}
