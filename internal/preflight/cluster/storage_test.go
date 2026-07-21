package cluster_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/preflight/cluster"
)

func storageClass(name string, annotations map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Annotations: annotations},
		Provisioner: "example.test/provisioner",
	}
}

func TestStorageClassDefault(t *testing.T) {
	t.Run("no StorageClass at all fails", func(t *testing.T) {
		res, err := probeCheck(t, cluster.Options{Client: fakeClient(t)}, cluster.IDStorageClassDefault)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "no StorageClass") {
			t.Errorf("message %q should say no StorageClass exists", res.Message)
		}
	})

	t.Run("classes without a default fail", func(t *testing.T) {
		c := fakeClient(t,
			storageClass("slow", nil),
			storageClass("fast", map[string]string{"storageclass.kubernetes.io/is-default-class": "false"}),
		)
		res, err := probeCheck(t, cluster.Options{Client: c}, cluster.IDStorageClassDefault)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "none is marked default") {
			t.Errorf("message %q should say none is marked default", res.Message)
		}
	})

	t.Run("GA default annotation passes", func(t *testing.T) {
		c := fakeClient(t,
			storageClass("slow", nil),
			storageClass("fast", map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}),
		)
		res, err := probeCheck(t, cluster.Options{Client: c}, cluster.IDStorageClassDefault)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "fast") {
			t.Errorf("message %q should name the default class", res.Message)
		}
	})

	t.Run("legacy beta default annotation passes", func(t *testing.T) {
		c := fakeClient(t,
			storageClass("legacy", map[string]string{"storageclass.beta.kubernetes.io/is-default-class": "true"}),
		)
		res, err := probeCheck(t, cluster.Options{Client: c}, cluster.IDStorageClassDefault)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
	})

	t.Run("multiple defaults pass naming each", func(t *testing.T) {
		c := fakeClient(t,
			storageClass("first", map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}),
			storageClass("second", map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}),
		)
		res, err := probeCheck(t, cluster.Options{Client: c}, cluster.IDStorageClassDefault)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "first") || !strings.Contains(res.Message, "second") {
			t.Errorf("message %q should name both defaults", res.Message)
		}
	})

	t.Run("list failure is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("apiserver unreachable")
				},
			}).Build()
		if _, err := probeCheck(t, cluster.Options{Client: c}, cluster.IDStorageClassDefault); err == nil {
			t.Fatal("expected a probe error")
		}
	})
}
