// The Postgres catalog kind's schema contract (ADR-0014), proven against
// envtest's real apiserver + CEL — no reconciler runs here (the catalog
// controller is the next M1.4 task), so these tests pin the apiserver-level
// guarantees the reconciler will build on: defaults, the immutable version
// transition, the enum bound, the quantity-typed storageSize, and the
// deliberate decision to leave storage growth unguarded in the schema.
package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func TestPostgresDefaults(t *testing.T) {
	ctx := context.Background()
	pg := &orkanov1alpha1.Postgres{
		ObjectMeta: metav1.ObjectMeta{Name: "defaults-probe", Namespace: appsNamespace},
	}
	if err := k8sClient.Create(ctx, pg); err != nil {
		t.Fatalf("failed to create bare Postgres: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pg) })

	var got orkanov1alpha1.Postgres
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "defaults-probe", Namespace: appsNamespace}, &got); err != nil {
		t.Fatalf("failed to get Postgres back: %v", err)
	}
	if got.Spec.Version != "16" {
		t.Errorf("version default = %q, want 16", got.Spec.Version)
	}
	if got.Spec.StorageSize == nil || got.Spec.StorageSize.String() != "10Gi" {
		t.Errorf("storageSize default = %v, want 10Gi", got.Spec.StorageSize)
	}
	if got.Spec.Pgweb != nil {
		t.Errorf("Pgweb should default to disabled: %+v", got.Spec.Pgweb)
	}
}

func TestPostgresVersionImmutable(t *testing.T) {
	ctx := context.Background()
	pg := &orkanov1alpha1.Postgres{
		ObjectMeta: metav1.ObjectMeta{Name: "immutable-version-probe", Namespace: appsNamespace},
		Spec:       orkanov1alpha1.PostgresSpec{Version: "16"},
	}
	if err := k8sClient.Create(ctx, pg); err != nil {
		t.Fatalf("failed to create Postgres: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pg) })

	pg.Spec.Version = "17"
	err := k8sClient.Update(ctx, pg)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "version is immutable") {
		t.Fatalf("expected the immutable-version CEL rule to reject, got: %v", err)
	}
}

func TestPostgresVersionEnum(t *testing.T) {
	ctx := context.Background()
	pg := &orkanov1alpha1.Postgres{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-version-probe", Namespace: appsNamespace},
		Spec:       orkanov1alpha1.PostgresSpec{Version: "13"},
	}
	err := k8sClient.Create(ctx, pg)
	// Pin the rejection to the enum specifically, not any incidental 422 — the
	// apiserver phrases an OpenAPI enum violation as "Unsupported value".
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "Unsupported value") {
		t.Fatalf("expected the version enum to reject 13 as an unsupported value, got: %v", err)
	}
}

// TestPostgresStorageSizeBadQuantity proves the resource.Quantity typing is
// enforced by the apiserver: a non-quantity string fails the int-or-string
// pattern. Submitted via unstructured because the typed *resource.Quantity
// can't hold an unparseable value (resource.MustParse would panic first). This
// is the go-test-level guard for hack/testdata/invalid/postgres-bad-storage.yaml.
func TestPostgresStorageSizeBadQuantity(t *testing.T) {
	ctx := context.Background()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "orkano.io/v1alpha1",
		"kind":       "Postgres",
		"metadata": map[string]any{
			"name":      "bad-storage-probe",
			"namespace": appsNamespace,
		},
		"spec": map[string]any{"storageSize": "ten-gigs"},
	}}
	err := k8sClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, obj) })
		t.Fatal("expected a non-quantity storageSize to be rejected, got accepted")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("expected an Invalid error for a malformed storageSize, got: %v", err)
	}
}

// TestPostgresStorageSizeSchemaUnguarded pins ADR-0014's decision: storage
// growth is enforced in the reconciler, not the schema (native PVC semantics),
// so the apiserver accepts BOTH a grow and a shrink. The reconciler — the next
// task — is what rejects a shrink onto the Ready condition.
func TestPostgresStorageSizeSchemaUnguarded(t *testing.T) {
	ctx := context.Background()
	pg := &orkanov1alpha1.Postgres{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-probe", Namespace: appsNamespace},
		Spec:       orkanov1alpha1.PostgresSpec{StorageSize: quantity(t, "10Gi")},
	}
	if err := k8sClient.Create(ctx, pg); err != nil {
		t.Fatalf("failed to create Postgres: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pg) })

	pg.Spec.StorageSize = quantity(t, "20Gi")
	if err := k8sClient.Update(ctx, pg); err != nil {
		t.Fatalf("schema rejected a storage grow, want accepted: %v", err)
	}
	pg.Spec.StorageSize = quantity(t, "5Gi")
	if err := k8sClient.Update(ctx, pg); err != nil {
		t.Fatalf("schema rejected a storage shrink (reconciler-side guard, not schema): %v", err)
	}
}
