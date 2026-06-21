package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

func createPostgres(t *testing.T, name string, mutate func(*orkanov1alpha1.Postgres)) *orkanov1alpha1.Postgres {
	t.Helper()
	ctx := context.Background()
	pg := &orkanov1alpha1.Postgres{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appsNamespace},
	}
	if mutate != nil {
		mutate(pg)
	}
	if err := k8sClient.Create(ctx, pg); err != nil {
		t.Fatalf("failed to create Postgres %s: %v", name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pg) })
	return pg
}

func getStatefulSet(t *testing.T, name string) *appsv1.StatefulSet {
	t.Helper()
	var sts appsv1.StatefulSet
	key := types.NamespacedName{Name: name, Namespace: appsNamespace}
	eventually(t, "StatefulSet "+name, func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, key, &sts)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	})
	return &sts
}

func getPostgresSecret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	var secret corev1.Secret
	key := types.NamespacedName{Name: name, Namespace: appsNamespace}
	eventually(t, "connection Secret "+name, func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, key, &secret)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	})
	return &secret
}

// markStatefulSetReady plays the StatefulSet controller, which envtest does not
// run: the reconciler gates Available on ReadyReplicas.
func markStatefulSetReady(t *testing.T, name string, ready int32) {
	t.Helper()
	eventually(t, "StatefulSet status for "+name, func(ctx context.Context) (bool, error) {
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appsNamespace}, &sts); err != nil {
			return false, err
		}
		sts.Status.ObservedGeneration = sts.Generation
		sts.Status.Replicas = ready
		sts.Status.ReadyReplicas = ready
		sts.Status.CurrentReplicas = ready
		sts.Status.UpdatedReplicas = ready
		sts.Status.AvailableReplicas = ready
		if err := k8sClient.Status().Update(ctx, &sts); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

func waitForPostgresCondition(t *testing.T, name, condType string, status metav1.ConditionStatus, reason string) *orkanov1alpha1.Postgres {
	t.Helper()
	var pg orkanov1alpha1.Postgres
	eventually(t, fmt.Sprintf("Postgres %s %s=%s/%s", name, condType, status, reason), func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appsNamespace}, &pg); err != nil {
			return false, err
		}
		cond := meta.FindStatusCondition(pg.Status.Conditions, condType)
		return cond != nil && cond.Status == status && cond.Reason == reason &&
			cond.ObservedGeneration == pg.Generation &&
			pg.Status.ObservedGeneration == pg.Generation, nil
	})
	return &pg
}

const expandableStorageClass = "orkano-test-expandable"

// createDataPVC plays the StatefulSet controller (which envtest does not run),
// creating the per-pod data PVC bound to an expansion-capable StorageClass —
// the apiserver's resize admission permits growth only on a bound claim whose
// StorageClass has allowVolumeExpansion, the common production case the
// reconciler relies on (it leaves storageClassName nil for the cluster default).
func createDataPVC(t *testing.T, postgresName, size string) {
	t.Helper()
	ctx := context.Background()
	allowExpansion := true
	sc := &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: expandableStorageClass},
		Provisioner:          "orkano.io/test",
		AllowVolumeExpansion: &allowExpansion,
	}
	if err := k8sClient.Create(ctx, sc); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("failed to create expandable StorageClass: %v", err)
	}
	scName := expandableStorageClass
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: postgresDataVolume + "-" + postgresName + "-0", Namespace: appsNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &scName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	if err := k8sClient.Create(ctx, pvc); err != nil {
		t.Fatalf("failed to create data PVC for %s: %v", postgresName, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pvc) })
	pvc.Status.Phase = corev1.ClaimBound
	if err := k8sClient.Status().Update(ctx, pvc); err != nil {
		t.Fatalf("failed to mark data PVC Bound for %s: %v", postgresName, err)
	}
}

func TestPostgresProvisioningObjectGraph(t *testing.T) {
	pg := createPostgres(t, "cat-db", func(p *orkanov1alpha1.Postgres) {
		p.Spec.Version = "16"
	})

	sts := getStatefulSet(t, pg.Name)
	assertOwnedBy(t, sts, "Postgres", pg.Name)
	if *sts.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", *sts.Spec.Replicas)
	}
	if sts.Spec.ServiceName != pg.Name {
		t.Errorf("serviceName = %q, want %q", sts.Spec.ServiceName, pg.Name)
	}
	if sts.Spec.Selector.MatchLabels[postgresLabel] != pg.Name {
		t.Errorf("selector = %+v, want %s=%s", sts.Spec.Selector, postgresLabel, pg.Name)
	}
	if rp := sts.Spec.PersistentVolumeClaimRetentionPolicy; rp == nil ||
		rp.WhenDeleted != appsv1.DeletePersistentVolumeClaimRetentionPolicyType {
		t.Errorf("PVC retention = %+v, want WhenDeleted=Delete (data is owned by the object)", sts.Spec.PersistentVolumeClaimRetentionPolicy)
	}
	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("volumeClaimTemplates = %+v, want exactly one", sts.Spec.VolumeClaimTemplates)
	}
	vct := sts.Spec.VolumeClaimTemplates[0]
	if vct.Name != postgresDataVolume {
		t.Errorf("VCT name = %q, want %q", vct.Name, postgresDataVolume)
	}
	if got := vct.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "10Gi" {
		t.Errorf("VCT storage = %s, want defaulted 10Gi", got.String())
	}

	sc := sts.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.FSGroup == nil || *sc.FSGroup != postgresUID {
		t.Errorf("pod securityContext = %+v, want runAsNonRoot + fsGroup %d", sc, postgresUID)
	}

	c := sts.Spec.Template.Spec.Containers[0]
	if c.Image != postgresImages["16"] {
		t.Errorf("image = %q, want the digest-pinned %q", c.Image, postgresImages["16"])
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != postgresPort || c.Ports[0].Name != postgresPortName {
		t.Errorf("ports = %+v, want named %q on %d", c.Ports, postgresPortName, postgresPort)
	}
	// INV-03: credentials come from the Secret by reference, never inline.
	wantRefs := map[string]string{
		"POSTGRES_USER":     orkanov1alpha1.SecretKeyUsername,
		"POSTGRES_PASSWORD": orkanov1alpha1.SecretKeyPassword,
		"POSTGRES_DB":       orkanov1alpha1.SecretKeyDatabase,
	}
	for name, key := range wantRefs {
		e := envEntries(c, name)
		if len(e) != 1 || e[0].Value != "" || e[0].ValueFrom == nil || e[0].ValueFrom.SecretKeyRef == nil ||
			e[0].ValueFrom.SecretKeyRef.Name != pg.Name || e[0].ValueFrom.SecretKeyRef.Key != key {
			t.Errorf("%s = %+v, want secretKeyRef %s/%s and no inline value", name, e, pg.Name, key)
		}
	}
	if pgdata := envEntries(c, "PGDATA"); len(pgdata) != 1 || pgdata[0].Value != postgresPGData {
		t.Errorf("PGDATA = %+v, want %q", pgdata, postgresPGData)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil ||
		strings.Join(c.ReadinessProbe.Exec.Command, " ") != "pg_isready -U cat_db -d cat_db" {
		t.Errorf("readiness probe = %+v, want pg_isready on the cat_db identifier", c.ReadinessProbe)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.TCPSocket == nil {
		t.Errorf("liveness probe = %+v, want TCPSocket", c.LivenessProbe)
	}
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("container securityContext = %+v, want allowPrivilegeEscalation false", c.SecurityContext)
	}

	// Headless Service.
	var svc corev1.Service
	eventually(t, "Service "+pg.Name, func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: pg.Name, Namespace: appsNamespace}, &svc)
		return err == nil, client.IgnoreNotFound(err)
	})
	assertOwnedBy(t, &svc, "Postgres", pg.Name)
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("clusterIP = %q, want None (headless)", svc.Spec.ClusterIP)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != postgresPort {
		t.Errorf("service ports = %+v, want %d", svc.Spec.Ports, postgresPort)
	}
	if svc.Spec.Selector[postgresLabel] != pg.Name {
		t.Errorf("service selector = %+v, want %s=%s", svc.Spec.Selector, postgresLabel, pg.Name)
	}

	// Connection Secret — the frozen ADR-0014 key contract.
	secret := getPostgresSecret(t, pg.Name)
	assertOwnedBy(t, secret, "Postgres", pg.Name)
	if secret.Type != corev1.SecretTypeOpaque {
		t.Errorf("secret type = %q, want Opaque", secret.Type)
	}
	host := "cat-db.orkano-apps.svc.cluster.local"
	wantData := map[string]string{
		orkanov1alpha1.SecretKeyHost:     host,
		orkanov1alpha1.SecretKeyPort:     "5432",
		orkanov1alpha1.SecretKeyDatabase: "cat_db",
		orkanov1alpha1.SecretKeyUsername: "cat_db",
	}
	for k, want := range wantData {
		if got := string(secret.Data[k]); got != want {
			t.Errorf("secret[%q] = %q, want %q", k, got, want)
		}
	}
	password := string(secret.Data[orkanov1alpha1.SecretKeyPassword])
	if password == "" {
		t.Error("secret password is empty")
	}
	wantURI := fmt.Sprintf("postgresql://cat_db:%s@%s:5432/cat_db", password, host)
	if got := string(secret.Data[orkanov1alpha1.SecretKeyURI]); got != wantURI {
		t.Errorf("secret uri = %q, want %q", got, wantURI)
	}
	// Exactly the 6 frozen keys — an extra key (e.g. a stray plaintext copy)
	// would be an INV-03 leak, so pin the count like internal/db pins columns.
	gotKeys := make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		gotKeys = append(gotKeys, k)
	}
	if len(gotKeys) != 6 {
		t.Errorf("secret has %d keys %v, want exactly 6 (ADR-0014 frozen contract)", len(gotKeys), gotKeys)
	}

	// status.secretName echoes the wiring; Ready is gated on the pod.
	got := waitForPostgresCondition(t, pg.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonProvisioning)
	if got.Status.SecretName != pg.Name {
		t.Errorf("status.secretName = %q, want %q", got.Status.SecretName, pg.Name)
	}

	markStatefulSetReady(t, pg.Name, 1)
	waitForPostgresCondition(t, pg.Name, orkanov1alpha1.ConditionReady, metav1.ConditionTrue, reasonAvailable)
}

func TestPostgresImagePerVersion(t *testing.T) {
	for _, v := range []string{"14", "15", "17"} {
		t.Run("v"+v, func(t *testing.T) {
			pg := createPostgres(t, "ver-"+v, func(p *orkanov1alpha1.Postgres) {
				p.Spec.Version = v
			})
			sts := getStatefulSet(t, pg.Name)
			if got := sts.Spec.Template.Spec.Containers[0].Image; got != postgresImages[v] {
				t.Errorf("image = %q, want %q", got, postgresImages[v])
			}
		})
	}
}

// assertNoChildObjects proves a rejected Postgres provisioned nothing — no
// StatefulSet, Service, OR Secret — so a future reordering that created the
// connection Secret (with a generated password) before the validation guard
// would leave an orphaned credential and fail this assertion (INV-03).
func assertNoChildObjects(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: appsNamespace}
	for _, obj := range []client.Object{&appsv1.StatefulSet{}, &corev1.Service{}, &corev1.Secret{}} {
		if err := k8sClient.Get(ctx, key, obj); !apierrors.IsNotFound(err) {
			t.Errorf("a %T was created for the rejected Postgres %q: %v", obj, name, err)
		}
	}
}

func TestPostgresInvalidNameProvisionFailed(t *testing.T) {
	// Names the DNS-1123-subdomain schema accepts but that cannot yield a valid
	// Service name / SQL identifier: a leading digit, an embedded dot, and a
	// 64-char name (passes the character class but exceeds PostgreSQL's 63-char
	// identifier limit, which would otherwise truncate silently).
	for _, name := range []string{"1db", "my.db", strings.Repeat("a", 64)} {
		t.Run(name, func(t *testing.T) {
			pg := createPostgres(t, name, nil)
			pgGot := waitForPostgresCondition(t, pg.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonProvisionFailed)
			cond := meta.FindStatusCondition(pgGot.Status.Conditions, orkanov1alpha1.ConditionReady)
			if !strings.Contains(cond.Message, "DNS-1035") {
				t.Errorf("message = %q, want the name rejection visible", cond.Message)
			}
			assertNoChildObjects(t, name)
		})
	}
}

func TestPostgresStorageTooSmall(t *testing.T) {
	small := resource.MustParse("500Mi")
	pg := createPostgres(t, "tiny-db", func(p *orkanov1alpha1.Postgres) {
		p.Spec.StorageSize = &small
	})
	got := waitForPostgresCondition(t, pg.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonProvisionFailed)
	cond := meta.FindStatusCondition(got.Status.Conditions, orkanov1alpha1.ConditionReady)
	if !strings.Contains(cond.Message, "minimum") {
		t.Errorf("message = %q, want the too-small rejection visible", cond.Message)
	}
	assertNoChildObjects(t, pg.Name)
}

func TestPostgresStorageGrowth(t *testing.T) {
	pg := createPostgres(t, "grow-db", nil)
	getStatefulSet(t, pg.Name)
	beforePassword := string(getPostgresSecret(t, pg.Name).Data[orkanov1alpha1.SecretKeyPassword])
	// envtest runs no StatefulSet controller, so play it: the per-pod data PVC.
	createDataPVC(t, pg.Name, "10Gi")

	var fresh orkanov1alpha1.Postgres
	eventually(t, "grow storageSize to 20Gi", func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: pg.Name, Namespace: appsNamespace}, &fresh); err != nil {
			return false, err
		}
		twenty := resource.MustParse("20Gi")
		fresh.Spec.StorageSize = &twenty
		if err := k8sClient.Update(ctx, &fresh); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})

	eventually(t, "data PVC grown to 20Gi", func(ctx context.Context) (bool, error) {
		var pvc corev1.PersistentVolumeClaim
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "data-grow-db-0", Namespace: appsNamespace}, &pvc); err != nil {
			return false, err
		}
		got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		return got.Cmp(resource.MustParse("20Gi")) == 0, nil
	})

	// A resize re-runs ensureSecret; the password must survive it (a regenerated
	// password would lock out the running database).
	if after := string(getPostgresSecret(t, pg.Name).Data[orkanov1alpha1.SecretKeyPassword]); after != beforePassword {
		t.Error("connection password changed across the storage resize")
	}
}

func TestPostgresStorageShrinkRejected(t *testing.T) {
	ctx := context.Background()
	pg := createPostgres(t, "shrink-db", nil)
	getStatefulSet(t, pg.Name)
	createDataPVC(t, pg.Name, "10Gi")

	var fresh orkanov1alpha1.Postgres
	eventually(t, "shrink storageSize to 5Gi", func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: pg.Name, Namespace: appsNamespace}, &fresh); err != nil {
			return false, err
		}
		five := resource.MustParse("5Gi")
		fresh.Spec.StorageSize = &five
		if err := k8sClient.Update(ctx, &fresh); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})

	got := waitForPostgresCondition(t, pg.Name, orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonProvisionFailed)
	cond := meta.FindStatusCondition(got.Status.Conditions, orkanov1alpha1.ConditionReady)
	if !strings.Contains(cond.Message, "shrink") {
		t.Errorf("message = %q, want the shrink rejection visible", cond.Message)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "data-shrink-db-0", Namespace: appsNamespace}, &pvc); err != nil {
		t.Fatalf("failed to read data PVC: %v", err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("PVC storage = %s after a rejected shrink, want unchanged 10Gi", got.String())
	}
}

func TestPostgresPasswordStableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	pg := createPostgres(t, "stable-db", nil)
	getStatefulSet(t, pg.Name)
	markStatefulSetReady(t, pg.Name, 1)
	waitForPostgresCondition(t, pg.Name, orkanov1alpha1.ConditionReady, metav1.ConditionTrue, reasonAvailable)

	password := string(getPostgresSecret(t, pg.Name).Data[orkanov1alpha1.SecretKeyPassword])

	rv := func(t *testing.T, obj client.Object) string {
		t.Helper()
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: pg.Name, Namespace: appsNamespace}, obj); err != nil {
			t.Fatalf("failed to read %T: %v", obj, err)
		}
		return obj.GetResourceVersion()
	}

	// Drift the StatefulSet's managed-by label, then wait for the reconciler to
	// restore it. This is a POSITIVE barrier — it proves a reconcile actually ran
	// (a bare sleep could pass vacuously if the controller was busy) — and proves
	// the mutate heals drift rather than ignoring it.
	eventually(t, "drift the StatefulSet label", func(ctx context.Context) (bool, error) {
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: pg.Name, Namespace: appsNamespace}, &sts); err != nil {
			return false, err
		}
		delete(sts.Labels, managedByLabel)
		if err := k8sClient.Update(ctx, &sts); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	eventually(t, "reconciler heals the StatefulSet label", func(ctx context.Context) (bool, error) {
		var sts appsv1.StatefulSet
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: pg.Name, Namespace: appsNamespace}, &sts); err != nil {
			return false, err
		}
		return sts.Labels[managedByLabel] == managedByValue, nil
	})

	// Now in steady state: Owns(StatefulSet) means a mutate that is not a no-op
	// against the stored object would re-trigger itself and climb resourceVersion
	// forever. Capture post-heal and prove both objects hold still across a window.
	secretRV := rv(t, &corev1.Secret{})
	stsRV := rv(t, &appsv1.StatefulSet{})
	time.Sleep(1500 * time.Millisecond)

	var freshSecret corev1.Secret
	if got := rv(t, &freshSecret); got != secretRV {
		t.Errorf("Secret resourceVersion churned from %s to %s with no input change", secretRV, got)
	}
	if got := string(freshSecret.Data[orkanov1alpha1.SecretKeyPassword]); got != password {
		t.Errorf("password changed across reconcile: regenerating it would lock out the running database")
	}
	var freshSTS appsv1.StatefulSet
	if got := rv(t, &freshSTS); got != stsRV {
		t.Errorf("StatefulSet resourceVersion churned from %s to %s — mutate is not a no-op against the stored object", stsRV, got)
	}
}
