package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func createMongo(t *testing.T, name string, mutate func(*orkanov1alpha1.Mongo)) *orkanov1alpha1.Mongo {
	t.Helper()
	mongo := &orkanov1alpha1.Mongo{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace}}
	if mutate != nil {
		mutate(mongo)
	}
	if err := k8sClient.Create(context.Background(), mongo); err != nil {
		t.Fatalf("create Mongo %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), mongo) })
	return mongo
}

func waitForMongoCondition(t *testing.T, name string, status metav1.ConditionStatus, reason string) *orkanov1alpha1.Mongo {
	t.Helper()
	var mongo orkanov1alpha1.Mongo
	eventually(t, fmt.Sprintf("Mongo %s Ready=%s/%s", name, status, reason), func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appsNamespace}, &mongo); err != nil {
			return false, err
		}
		cond := meta.FindStatusCondition(mongo.Status.Conditions, orkanov1alpha1.ConditionReady)
		return cond != nil && cond.Status == status && cond.Reason == reason &&
			cond.ObservedGeneration == mongo.Generation && mongo.Status.ObservedGeneration == mongo.Generation, nil
	})
	return &mongo
}

func TestMongoDefaultsAndVersionContract(t *testing.T) {
	ctx := context.Background()
	mongo := &orkanov1alpha1.Mongo{ObjectMeta: metav1.ObjectMeta{Name: "mongo-defaults", Namespace: appsNamespace}}
	if err := k8sClient.Create(ctx, mongo); err != nil {
		t.Fatalf("create bare Mongo: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, mongo) })
	var got orkanov1alpha1.Mongo
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: mongo.Name, Namespace: appsNamespace}, &got); err != nil {
		t.Fatalf("get Mongo: %v", err)
	}
	if got.Spec.Version != "8.0" || got.Spec.StorageSize == nil || got.Spec.StorageSize.String() != "10Gi" {
		t.Fatalf("defaults = %+v, want 8.0/10Gi", got.Spec)
	}
	got.Spec.Version = "8.2"
	err := k8sClient.Update(ctx, &got)
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "Unsupported value") {
		t.Fatalf("unsupported version accepted: %v", err)
	}
}

func TestMongoProvisioningObjectGraph(t *testing.T) {
	mongo := createMongo(t, "document-db", nil)
	sts := getStatefulSet(t, mongo.Name)
	assertOwnedBy(t, sts, "Mongo", mongo.Name)
	if sts.Spec.ServiceName != mongo.Name || sts.Spec.Selector.MatchLabels[mongoLabel] != mongo.Name {
		t.Fatalf("StatefulSet identity = service %q selector %v", sts.Spec.ServiceName, sts.Spec.Selector)
	}
	if rp := sts.Spec.PersistentVolumeClaimRetentionPolicy; rp == nil || rp.WhenDeleted != appsv1.DeletePersistentVolumeClaimRetentionPolicyType {
		t.Errorf("PVC retention = %+v, want delete with Mongo", rp)
	}
	if got := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "10Gi" {
		t.Errorf("storage = %s, want 10Gi", got.String())
	}
	c := sts.Spec.Template.Spec.Containers[0]
	if c.Image != mongoImages["8.0"] {
		t.Errorf("image = %q, want %q", c.Image, mongoImages["8.0"])
	}
	wantRefs := map[string]string{
		"MONGO_INITDB_ROOT_USERNAME": orkanov1alpha1.SecretKeyUsername,
		"MONGO_INITDB_ROOT_PASSWORD": orkanov1alpha1.SecretKeyPassword,
		"MONGO_INITDB_DATABASE":      orkanov1alpha1.SecretKeyDatabase,
	}
	for name, key := range wantRefs {
		env := envEntries(c, name)
		if len(env) != 1 || env[0].Value != "" || env[0].ValueFrom == nil || env[0].ValueFrom.SecretKeyRef == nil ||
			env[0].ValueFrom.SecretKeyRef.Name != mongo.Name || env[0].ValueFrom.SecretKeyRef.Key != key {
			t.Errorf("%s = %+v, want Secret %s/%s", name, env, mongo.Name, key)
		}
	}
	if c.StartupProbe == nil || c.StartupProbe.TCPSocket == nil || c.ReadinessProbe == nil || c.LivenessProbe == nil {
		t.Errorf("probes = startup %+v readiness %+v liveness %+v", c.StartupProbe, c.ReadinessProbe, c.LivenessProbe)
	}

	var svc corev1.Service
	eventually(t, "Mongo Service", func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: mongo.Name, Namespace: appsNamespace}, &svc)
		return err == nil, client.IgnoreNotFound(err)
	})
	assertOwnedBy(t, &svc, "Mongo", mongo.Name)
	if svc.Spec.ClusterIP != corev1.ClusterIPNone || svc.Spec.Ports[0].Port != mongoPort {
		t.Errorf("Service = clusterIP %q ports %v", svc.Spec.ClusterIP, svc.Spec.Ports)
	}

	secret := getPostgresSecret(t, mongo.Name)
	assertOwnedBy(t, secret, "Mongo", mongo.Name)
	host := "document-db.orkano-apps.svc.cluster.local"
	password := string(secret.Data[orkanov1alpha1.SecretKeyPassword])
	wantURI := fmt.Sprintf("mongodb://document_db:%s@%s:27017/document_db?authSource=admin", password, host)
	if got := string(secret.Data[orkanov1alpha1.SecretKeyURI]); got != wantURI {
		t.Errorf("uri = %q, want %q", got, wantURI)
	}
	if len(secret.Data) != 6 || string(secret.Data[orkanov1alpha1.SecretKeyHost]) != host ||
		string(secret.Data[orkanov1alpha1.SecretKeyPort]) != "27017" ||
		string(secret.Data[orkanov1alpha1.SecretKeyDatabase]) != "document_db" {
		t.Errorf("connection Secret contract = %v", secret.Data)
	}

	got := waitForMongoCondition(t, mongo.Name, metav1.ConditionFalse, reasonProvisioning)
	if got.Status.SecretName != mongo.Name {
		t.Errorf("secretName = %q, want %q", got.Status.SecretName, mongo.Name)
	}
	markStatefulSetReady(t, mongo.Name, 1)
	waitForMongoCondition(t, mongo.Name, metav1.ConditionTrue, reasonAvailable)
}

func TestMongoStorageGrowAndShrinkGuard(t *testing.T) {
	mongo := createMongo(t, "mongo-storage", nil)
	_ = getStatefulSet(t, mongo.Name)
	createDataPVC(t, mongo.Name, "10Gi")

	eventually(t, "Mongo storage grow", func(ctx context.Context) (bool, error) {
		var current orkanov1alpha1.Mongo
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: mongo.Name, Namespace: appsNamespace}, &current); err != nil {
			return false, err
		}
		current.Spec.StorageSize = quantity(t, "20Gi")
		if err := k8sClient.Update(ctx, &current); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return true, nil
	})
	eventually(t, "Mongo PVC at 20Gi", func(ctx context.Context) (bool, error) {
		var pvc corev1.PersistentVolumeClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: mongoDataVolume + "-" + mongo.Name + "-0", Namespace: appsNamespace}, &pvc); err != nil {
			return false, err
		}
		got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		return got.String() == "20Gi", nil
	})

	var current orkanov1alpha1.Mongo
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: mongo.Name, Namespace: appsNamespace}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.StorageSize = quantity(t, "5Gi")
	if err := k8sClient.Update(context.Background(), &current); err != nil {
		t.Fatalf("schema should accept shrink for reconciler guard: %v", err)
	}
	failed := waitForMongoCondition(t, mongo.Name, metav1.ConditionFalse, reasonProvisionFailed)
	if message := meta.FindStatusCondition(failed.Status.Conditions, orkanov1alpha1.ConditionReady).Message; !strings.Contains(message, "cannot shrink") {
		t.Errorf("failure message = %q", message)
	}
}

func TestMongoCrossKindConflictDoesNotTakeOverPostgres(t *testing.T) {
	pg := createPostgres(t, "catalog-collision", nil)
	_ = getStatefulSet(t, pg.Name)

	mongo := createMongo(t, pg.Name, nil)
	failed := waitForMongoCondition(t, mongo.Name, metav1.ConditionFalse, reasonProvisionFailed)
	condition := meta.FindStatusCondition(failed.Status.Conditions, orkanov1alpha1.ConditionReady)
	if condition == nil || !strings.Contains(condition.Message, "owned by Postgres catalog-collision") {
		t.Fatalf("Mongo conflict condition = %+v, want existing Postgres owner", condition)
	}

	secret := getPostgresSecret(t, pg.Name)
	assertOwnedBy(t, secret, "Postgres", pg.Name)
	if uri := string(secret.Data[orkanov1alpha1.SecretKeyURI]); !strings.HasPrefix(uri, "postgresql://") {
		t.Errorf("connection Secret was overwritten by Mongo: %q", uri)
	}
}
