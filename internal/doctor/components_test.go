package doctor_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/doctor"
)

func probeComponents(t *testing.T, c client.Client) (check.Result, error) {
	t.Helper()
	for _, ck := range doctor.Checks(doctor.Options{Client: c}) {
		if ck.ID == doctor.IDComponentsReady {
			return ck.Probe(context.Background())
		}
	}
	t.Fatalf("check %s not registered", doctor.IDComponentsReady)
	return check.Result{}, nil
}

func readyDeployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-system", Name: name},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
}

func readyStatefulSet(name string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orkano-system", Name: name},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
}

// healthyComponents is the full set of ready orkano-system control-plane
// workloads the components check reads.
func healthyComponents() []client.Object {
	return []client.Object{
		readyDeployment("orkano-operator"),
		readyDeployment("orkano-receiver"),
		readyDeployment("orkano-registry"),
		readyDeployment("orkano-dashboard"),
		readyStatefulSet("orkano-postgres"),
	}
}

func TestComponentsReady(t *testing.T) {
	t.Run("all ready passes", func(t *testing.T) {
		res, err := probeComponents(t, fakeClient(t, healthyComponents()...))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
	})

	t.Run("missing workloads fail and aggregate", func(t *testing.T) {
		// Only three of the four Deployments present, no StatefulSet.
		c := fakeClient(t,
			readyDeployment("orkano-operator"),
			readyDeployment("orkano-receiver"),
			readyDeployment("orkano-registry"),
		)
		res, err := probeComponents(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q, want fail", res.Status)
		}
		for _, want := range []string{"orkano-dashboard", "orkano-postgres"} {
			if !strings.Contains(res.Message, want) {
				t.Errorf("aggregated message missing %s: %s", want, res.Message)
			}
		}
	})

	t.Run("unready replicas fail", func(t *testing.T) {
		objs := healthyComponents()
		objs[0].(*appsv1.Deployment).Status.ReadyReplicas = 0
		res, err := probeComponents(t, fakeClient(t, objs...))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q, want fail", res.Status)
		}
		if !strings.Contains(res.Message, "orkano-operator") || !strings.Contains(res.Message, "no ready replicas") {
			t.Errorf("message %q should name the unready Deployment", res.Message)
		}
	})

	// The StatefulSet comparison is a separate loop from the Deployments — cover
	// a present-but-zero-ready Postgres so a regression there cannot slip past.
	t.Run("unready statefulset fails", func(t *testing.T) {
		objs := healthyComponents()
		objs[4].(*appsv1.StatefulSet).Status.ReadyReplicas = 0
		res, err := probeComponents(t, fakeClient(t, objs...))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q, want fail", res.Status)
		}
		if !strings.Contains(res.Message, "orkano-postgres") || !strings.Contains(res.Message, "no ready replicas") {
			t.Errorf("message %q should name the unready StatefulSet", res.Message)
		}
	})

	// A read the cluster refuses is indeterminate — a probe error, never a
	// definitive fail (unknown never counts as hardened).
	t.Run("read failure is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("apiserver unreachable")
				},
			}).Build()
		if _, err := probeComponents(t, c); err == nil {
			t.Fatal("expected a probe error")
		}
	})
}
