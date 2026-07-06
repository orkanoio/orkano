package cluster_test

import (
	"context"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/checks"
	"github.com/orkanoio/orkano/internal/preflight/cluster"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("build scheme: %v", err)
	}
	return scheme
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
}

// probeCheck runs the identified check's Probe against the given options.
func probeCheck(t *testing.T, opt cluster.Options, id string) (check.Result, error) {
	t.Helper()
	for _, ck := range cluster.Checks(opt) {
		if ck.ID == id {
			return ck.Probe(context.Background())
		}
	}
	t.Fatalf("check %s not registered", id)
	return check.Result{}, nil
}

// TestChecksContract pins the shipped check metadata: IDs are permanent
// (--json/CI), severities gate the exit code, and none carries Requires or a
// Fix — the preflight refuses, it never remediates someone else's cluster.
func TestChecksContract(t *testing.T) {
	want := []struct {
		id       string
		severity check.Severity
	}{
		{"cluster.version-supported", check.SeverityCritical},
		{"cluster.storageclass-default", check.SeverityCritical},
		{"cluster.ingressclass-present", check.SeverityCritical},
		{"cluster.rbac-sufficient", check.SeverityCritical},
	}
	cs := cluster.Checks(cluster.Options{})
	if len(cs) != len(want) {
		t.Fatalf("Checks() returned %d checks, want %d", len(cs), len(want))
	}
	for i, w := range want {
		c := cs[i]
		if c.ID != w.id {
			t.Errorf("check %d ID = %q, want %q", i, c.ID, w.id)
		}
		if c.Severity != w.severity {
			t.Errorf("%s severity = %q, want %q", c.ID, c.Severity, w.severity)
		}
		if len(c.Requires) != 0 {
			t.Errorf("%s: unexpected Requires %v", c.ID, c.Requires)
		}
		if c.Fix != nil {
			t.Errorf("%s: the cluster preflight never remediates a BYO cluster; Fix must be nil", c.ID)
		}
		if c.Probe == nil || c.Summary == "" || c.Remediation == "" {
			t.Errorf("%s: Probe, Summary and Remediation must all be set", c.ID)
		}
	}
}

// TestRegisterRunsHealthyCluster drives the whole set through a real
// checks.Registry against a healthy fake cluster — the wiring the future
// `orkano preflight` command reuses (Register's error propagation included).
func TestRegisterRunsHealthyCluster(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(
			storageClass("standard", map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}),
			ingressClass("traefik", "true"),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if ssar, ok := obj.(*authorizationv1.SelfSubjectAccessReview); ok {
					ssar.Status.Allowed = true
					return nil
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	opt := cluster.Options{Client: c, ServerVersion: stubVersion("1", "36", "v1.36.0")}

	reg := checks.New()
	if err := cluster.Register(reg, opt); err != nil {
		t.Fatalf("Register: %v", err)
	}
	run, err := reg.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(run.Results) != 4 {
		t.Fatalf("run produced %d results, want 4", len(run.Results))
	}
	for _, res := range run.Results {
		if res.Outcome != checks.OutcomePass {
			t.Errorf("%s outcome = %q (%s), want pass", res.ID, res.Outcome, res.Message)
		}
	}
	if !run.OK() {
		t.Error("a healthy cluster must clear the preflight gate")
	}
}
