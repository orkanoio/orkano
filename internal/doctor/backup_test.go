package doctor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/api/check"
	"github.com/orkanoio/orkano/internal/doctor"
)

// snapshotFile builds an unstructured k3s.cattle.io/v1 ETCDSnapshotFile.
// created zero = no status.creationTime (in flight); errMsg non-empty = a
// failed attempt.
func snapshotFile(name, node string, created time.Time, errMsg string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "k3s.cattle.io", Version: "v1", Kind: "ETCDSnapshotFile"})
	u.SetName(name)
	if err := unstructured.SetNestedField(u.Object, node, "spec", "nodeName"); err != nil {
		panic(err)
	}
	if !created.IsZero() {
		if err := unstructured.SetNestedField(u.Object, created.Format(time.RFC3339), "status", "creationTime"); err != nil {
			panic(err)
		}
	}
	if errMsg != "" {
		if err := unstructured.SetNestedMap(u.Object, map[string]interface{}{"message": errMsg}, "status", "error"); err != nil {
			panic(err)
		}
	}
	return u
}

func etcdNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Annotations: map[string]string{"etcd.k3s.cattle.io/node-name": name + "-member"},
	}}
}

func kubeSystemNS(created time.Time) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:              "kube-system",
		CreationTimestamp: metav1.NewTime(created),
	}}
}

func TestEtcdSnapshotAge(t *testing.T) {
	probe := func(t *testing.T, c client.Client) (check.Result, error) {
		t.Helper()
		return probeCheck(t, doctor.Options{Client: c}, doctor.IDEtcdSnapshotAge)
	}

	t.Run("fresh snapshot passes", func(t *testing.T) {
		c := fakeClient(t,
			snapshotFile("etcd-snapshot-node1-1", "node1", testNow.Add(-6*time.Hour), ""),
			snapshotFile("etcd-snapshot-node1-0", "node1", testNow.Add(-18*time.Hour), ""),
		)
		res, err := probe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "etcd-snapshot-node1-1") || !strings.Contains(res.Message, "6h ago") {
			t.Errorf("message %q should name the newest snapshot and its age", res.Message)
		}
	})

	// ETCDSnapshotFile is cluster-scoped so records from every server mix in
	// one list; the newest must win across nodes, not per node.
	t.Run("newest snapshot is selected across nodes", func(t *testing.T) {
		c := fakeClient(t,
			snapshotFile("etcd-snapshot-node1-0", "node1", testNow.Add(-40*time.Hour), ""),
			snapshotFile("etcd-snapshot-node2-0", "node2", testNow.Add(-3*time.Hour), ""),
			snapshotFile("etcd-snapshot-node3-0", "node3", testNow.Add(-15*time.Hour), ""),
		)
		res, err := probe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "node2") || !strings.Contains(res.Message, "3h ago") {
			t.Errorf("message %q should name node2's snapshot as the newest", res.Message)
		}
	})

	t.Run("stale newest snapshot fails", func(t *testing.T) {
		c := fakeClient(t, snapshotFile("etcd-snapshot-node1-0", "node1", testNow.Add(-30*time.Hour), ""))
		res, err := probe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "30h old") {
			t.Errorf("message %q should carry the stale age", res.Message)
		}
	})

	t.Run("custom max age is honoured", func(t *testing.T) {
		c := fakeClient(t, snapshotFile("s", "node1", testNow.Add(-30*time.Hour), ""))
		res, err := probeCheck(t, doctor.Options{Client: c, MaxSnapshotAge: 48 * time.Hour}, doctor.IDEtcdSnapshotAge)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass under a 48h limit", res.Status, res.Message)
		}
	})

	// A single failed attempt must not fail the check while a fresh usable
	// snapshot exists — but failed attempts never count as usable either.
	t.Run("failed newest attempt with fresh usable snapshot passes", func(t *testing.T) {
		c := fakeClient(t,
			snapshotFile("bad", "node1", time.Time{}, "context deadline exceeded"),
			snapshotFile("good", "node1", testNow.Add(-2*time.Hour), ""),
		)
		res, err := probe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass", res.Status, res.Message)
		}
	})

	t.Run("only failed attempts fail with a sample error", func(t *testing.T) {
		c := fakeClient(t,
			snapshotFile("bad-0", "node1", time.Time{}, "disk full"),
			snapshotFile("bad-1", "node1", time.Time{}, "connection reset"),
		)
		res, err := probe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		// List order carries no chronology, so the message claims only a
		// sample — either record's error is a correct one.
		if !strings.Contains(res.Message, "sample error:") ||
			(!strings.Contains(res.Message, "disk full") && !strings.Contains(res.Message, "connection reset")) {
			t.Errorf("message %q should surface one snapshot error as a sample", res.Message)
		}
	})

	// A failed attempt is excluded from freshness even when it carries a
	// creationTime: its fresh timestamp must not mask that the last USABLE
	// snapshot is stale.
	t.Run("errored snapshot with a fresh timestamp does not count as usable", func(t *testing.T) {
		c := fakeClient(t,
			snapshotFile("fresh-but-failed", "node1", testNow.Add(-1*time.Hour), "apply failed"),
			snapshotFile("old-but-usable", "node1", testNow.Add(-30*time.Hour), ""),
		)
		res, err := probe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail — the usable snapshot is stale", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "old-but-usable") {
			t.Errorf("message %q should name the newest USABLE snapshot", res.Message)
		}
	})

	// Exactly at the limit counts as within it, on both paths.
	t.Run("snapshot exactly at the age limit passes", func(t *testing.T) {
		c := fakeClient(t, snapshotFile("s", "node1", testNow.Add(-25*time.Hour), ""))
		res, err := probe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusPass {
			t.Fatalf("status = %q (%s), want pass at the exact boundary", res.Status, res.Message)
		}
	})

	t.Run("zero snapshots with cluster age exactly at the limit skips", func(t *testing.T) {
		res, err := probe(t, fakeClient(t, etcdNode("node1"), kubeSystemNS(testNow.Add(-25*time.Hour))))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusSkip {
			t.Fatalf("status = %q (%s), want skip at the exact boundary", res.Status, res.Message)
		}
	})

	t.Run("zero snapshots without etcd members skips", func(t *testing.T) {
		plain := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "byo-node"}}
		res, err := probe(t, fakeClient(t, plain, kubeSystemNS(testNow.Add(-100*time.Hour))))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusSkip {
			t.Fatalf("status = %q (%s), want skip", res.Status, res.Message)
		}
	})

	t.Run("zero snapshots on a young etcd cluster skips", func(t *testing.T) {
		res, err := probe(t, fakeClient(t, etcdNode("node1"), kubeSystemNS(testNow.Add(-2*time.Hour))))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusSkip {
			t.Fatalf("status = %q (%s), want skip", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "first scheduled window") {
			t.Errorf("message %q should explain the young-cluster allowance", res.Message)
		}
	})

	t.Run("zero snapshots on an old etcd cluster fails", func(t *testing.T) {
		res, err := probe(t, fakeClient(t, etcdNode("node1"), kubeSystemNS(testNow.Add(-100*time.Hour))))
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusFail {
			t.Fatalf("status = %q (%s), want fail", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "zero snapshots") {
			t.Errorf("message %q should state that no snapshots exist", res.Message)
		}
	})

	t.Run("absent snapshot CRD skips", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind().Group == "k3s.cattle.io" {
						return &meta.NoKindMatchError{GroupKind: u.GroupVersionKind().GroupKind()}
					}
					return cl.List(ctx, list, opts...)
				},
			}).Build()
		res, err := probe(t, c)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if res.Status != check.StatusSkip {
			t.Fatalf("status = %q (%s), want skip", res.Status, res.Message)
		}
	})

	t.Run("snapshot list failure is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("apiserver unreachable")
				},
			}).Build()
		if _, err := probe(t, c); err == nil {
			t.Fatal("expected a probe error")
		}
	})

	t.Run("kube-system read failure is a probe error", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(etcdNode("node1")).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return errors.New("apiserver unreachable")
				},
			}).Build()
		if _, err := probe(t, c); err == nil {
			t.Fatal("expected a probe error")
		}
	})

	t.Run("malformed creationTime is a probe error", func(t *testing.T) {
		bad := snapshotFile("garbled", "node1", time.Time{}, "")
		if err := unstructured.SetNestedField(bad.Object, "yesterday-ish", "status", "creationTime"); err != nil {
			t.Fatal(err)
		}
		_, err := probe(t, fakeClient(t, bad))
		if err == nil || !strings.Contains(err.Error(), "garbled") {
			t.Fatalf("expected a probe error naming the snapshot, got %v", err)
		}
	})
}
