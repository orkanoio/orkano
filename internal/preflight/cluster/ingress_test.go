package cluster_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/preflight/cluster"
)

func ingressClass(name string, defaultAnnotation string) *networkingv1.IngressClass {
	ic := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       networkingv1.IngressClassSpec{Controller: "example.test/controller"},
	}
	if defaultAnnotation != "" {
		ic.Annotations = map[string]string{"ingressclass.kubernetes.io/is-default-class": defaultAnnotation}
	}
	return ic
}

func TestIngressClassPresent(t *testing.T) {
	t.Run("no IngressClass fails", func(t *testing.T) {
		res, err := probeCheck(t, cluster.Options{Client: fakeClient(t)}, cluster.IDIngressClassPresent)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "no IngressClass") {
			t.Errorf("message %q should say no IngressClass exists", res.Message)
		}
	})

	// No default class is an install-values choice (ingress.className), not a
	// cluster defect — a pass that carries the action, never a refusal.
	t.Run("classes without a default pass carrying the action", func(t *testing.T) {
		// An explicit "false" annotation must not count as default (the
		// equality check, not mere annotation presence).
		c := fakeClient(t, ingressClass("traefik", ""), ingressClass("nginx", "false"))
		res, err := probeCheck(t, cluster.Options{Client: c}, cluster.IDIngressClassPresent)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "none is default") || !strings.Contains(res.Message, "ingress.className") {
			t.Errorf("message %q should say none is default and name the ingress.className value", res.Message)
		}
	})

	t.Run("a default class passes", func(t *testing.T) {
		c := fakeClient(t, ingressClass("traefik", "true"), ingressClass("nginx", ""))
		res, err := probeCheck(t, cluster.Options{Client: c}, cluster.IDIngressClassPresent)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "traefik") {
			t.Errorf("message %q should name the default class", res.Message)
		}
		if strings.Contains(res.Message, "none is default") {
			t.Errorf("message %q should not carry the no-default guidance", res.Message)
		}
	})

	t.Run("list failure is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("apiserver unreachable")
				},
			}).Build()
		if _, err := probeCheck(t, cluster.Options{Client: c}, cluster.IDIngressClassPresent); err == nil {
			t.Fatal("expected a probe error")
		}
	})
}
