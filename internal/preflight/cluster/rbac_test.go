package cluster_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/preflight/cluster"
)

// ssarClient builds a client whose SelfSubjectAccessReview creates are
// answered by deny — the fake client cannot evaluate authorization, so the
// interceptor plays the apiserver (the doctor canary-status idiom).
func ssarClient(t *testing.T, deny func(*authorizationv1.ResourceAttributes) bool) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				ssar, ok := obj.(*authorizationv1.SelfSubjectAccessReview)
				if !ok {
					return cl.Create(ctx, obj, opts...)
				}
				attrs := ssar.Spec.ResourceAttributes
				if attrs == nil || attrs.Verb != "create" {
					t.Errorf("unexpected SSAR spec: %+v", ssar.Spec)
				}
				ssar.Status.Allowed = !deny(attrs)
				return nil
			},
		}).Build()
}

func probeRBAC(t *testing.T, deny func(*authorizationv1.ResourceAttributes) bool) (check.Result, error) {
	t.Helper()
	return probeCheck(t, cluster.Options{Client: ssarClient(t, deny)}, cluster.IDRBACSufficient)
}

func TestRBACSufficient(t *testing.T) {
	allowAll := func(*authorizationv1.ResourceAttributes) bool { return false }

	t.Run("everything allowed passes", func(t *testing.T) {
		res, err := probeRBAC(t, allowAll)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
	})

	t.Run("a denied cluster-scoped resource fails naming it", func(t *testing.T) {
		res, err := probeRBAC(t, func(a *authorizationv1.ResourceAttributes) bool {
			return a.Resource == "customresourcedefinitions"
		})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "customresourcedefinitions.apiextensions.k8s.io (cluster-scoped)") {
			t.Errorf("message %q should name the denied cluster-scoped resource", res.Message)
		}
	})

	// Behavioral membership pins for load-bearing tuples: denying exactly one
	// must flip the verdict (and name the tuple), proving the walk covers it.
	membership := []struct {
		name        string
		group       string
		resource    string
		namespace   string
		wantMessage string
	}{
		{name: "helm release storage (secrets in orkano-system)", resource: "secrets", namespace: "orkano-system",
			wantMessage: "secrets in orkano-system"},
		{name: "lockdown netpol in orkano-builds", group: "networking.k8s.io", resource: "networkpolicies", namespace: "orkano-builds",
			wantMessage: "networkpolicies.networking.k8s.io in orkano-builds"},
		{name: "operator RBAC in orkano-apps", group: "rbac.authorization.k8s.io", resource: "roles", namespace: "orkano-apps"},
		{name: "vendored cert-manager deploys in its namespace", group: "apps", resource: "deployments", namespace: "cert-manager"},
		{name: "the node-prep DaemonSet (ADR-0019 decision 3)", group: "apps", resource: "daemonsets", namespace: "orkano-system"},
		{name: "the orkano-platform ClusterIssuer", group: "cert-manager.io", resource: "clusterissuers", namespace: ""},
	}
	for _, m := range membership {
		t.Run("denying "+m.name+" fails", func(t *testing.T) {
			res, err := probeRBAC(t, func(a *authorizationv1.ResourceAttributes) bool {
				return a.Group == m.group && a.Resource == m.resource && a.Namespace == m.namespace
			})
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if res.Status != check.StatusFail {
				t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
			}
			if m.wantMessage != "" && !strings.Contains(res.Message, m.wantMessage) {
				t.Errorf("message %q should name the denied tuple as %q", res.Message, m.wantMessage)
			}
		})
	}

	// The walk asks only for what the install writes — denying kinds outside
	// the set (pods, and app CRs the operator owns post-install) must not fail
	// the preflight.
	t.Run("denying resources outside the install set still passes", func(t *testing.T) {
		res, err := probeRBAC(t, func(a *authorizationv1.ResourceAttributes) bool {
			return a.Resource == "pods" || a.Group == "orkano.io"
		})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
	})

	// The exact totals double as the tuple-count pin: dropping a resource kind
	// or a namespace from installAccessSet changes "67 of 67" and "+59 more"
	// and must be a deliberate edit here too.
	t.Run("denying everything caps the listed tuples with exact totals", func(t *testing.T) {
		res, err := probeRBAC(t, func(*authorizationv1.ResourceAttributes) bool { return true })
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q, want fail", res.Status)
		}
		if !strings.Contains(res.Message, "67 of 67") {
			t.Errorf("message %q should report the full tuple count as 67 of 67", res.Message)
		}
		if !strings.Contains(res.Message, "(+59 more)") {
			t.Errorf("message %q should cap the list at 8 with (+59 more)", res.Message)
		}
	})

	t.Run("exactly the cap of denials lists all of them with no more-suffix", func(t *testing.T) {
		// All 7 cluster-scoped tuples plus one namespaced = 8 == maxListed.
		res, err := probeRBAC(t, func(a *authorizationv1.ResourceAttributes) bool {
			return a.Namespace == "" || (a.Resource == "secrets" && a.Namespace == "orkano-system")
		})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q, want fail", res.Status)
		}
		if !strings.Contains(res.Message, "8 of 67") {
			t.Errorf("message %q should report 8 of 67 denied", res.Message)
		}
		if strings.Contains(res.Message, "more)") {
			t.Errorf("message %q should list all 8 without a +N more suffix", res.Message)
		}
	})

	t.Run("SSAR create failure is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					return errors.New("apiserver unreachable")
				},
			}).Build()
		if _, err := probeCheck(t, cluster.Options{Client: c}, cluster.IDRBACSufficient); err == nil {
			t.Fatal("expected a probe error")
		}
	})
}
