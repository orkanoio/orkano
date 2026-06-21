package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

const (
	// postgresLabel keys the StatefulSet's immutable selector and the headless
	// Service selector; renaming it would orphan every running database.
	postgresLabel = "app.orkano.io/postgres"

	postgresContainerName = "postgres"
	postgresPort          = int32(5432)
	postgresPortName      = "postgres"
	// postgresDataVolume is the volumeClaimTemplate name; the per-pod PVC the
	// StatefulSet controller derives is "<volume>-<sts>-<ordinal>", so the
	// single-replica data PVC the reconciler grows is "data-<name>-0".
	postgresDataVolume = "data"
	postgresDataMount  = "/var/lib/postgresql/data"
	// PGDATA is a subdirectory of the mount: the official image creates it with
	// the 0700 perms initdb demands, sidestepping the group-writable mount that
	// fsGroup leaves on the volume root ("data directory has group or world
	// access" otherwise).
	postgresPGData = postgresDataMount + "/pgdata"
	// The image runs as the stock postgres uid; fsGroup makes the PVC writable
	// to it under the restricted-grade securityContext orkano-apps warns on.
	postgresUID = int64(999)

	// maxIdentifierLen is PostgreSQL's NAMEDATALEN-1: identifiers longer than
	// this are silently truncated by initdb, so a name above it is rejected
	// rather than left to diverge from the connection Secret.
	maxIdentifierLen = 63

	reasonProvisioning    = "Provisioning"
	reasonProvisionFailed = "ProvisionFailed"
	// reasonAvailable and reasonReconcileError are shared with the App controller:
	// Available is the terminal Ready reason; ReconcileError marks a transient
	// infrastructure error, kept distinct from the permanent ProvisionFailed.
)

// postgresImages maps each api/v1alpha1 PostgresSpec.Version enum value to a
// digest-pinned, multi-arch (amd64+arm64) image index. ADR-0014 requires an
// entry for every value the enum serves — a missing one is a ProvisionFailed,
// not a panic. Bump deliberately (Renovate does not rewrite Go map literals);
// re-resolve the INDEX digest, not a single-platform manifest, or it breaks on
// the other arch (`docker buildx imagetools inspect postgres:<v>`).
var postgresImages = map[string]string{
	"14": "postgres:14@sha256:d462928b1898dd74b749ef486797968828c1e7fc9befb5e5ca03a33bfbc32d64",
	"15": "postgres:15@sha256:c2ca90969ca293925ab474466e837689cc712321afdd9e4640bd0cf942fdca3a",
	"16": "postgres:16@sha256:081f1bc7bd5e143dbb6e487b710bbc27712cdcfaced4c071b8e47349aa1b4171",
	"17": "postgres:17@sha256:2203e6282d9e7de7c24d7da234e2a744fb325df366a3fd8ed940e8abbee39527",
}

// defaultStorageSize mirrors the CRD default so an object created without the
// apiserver defaulter (e.g. a unit test) still provisions sanely.
var defaultStorageSize = resource.MustParse("10Gi")

// minStorageSize is the floor below which Postgres cannot reliably initdb and
// hold WAL — a too-small request surfaces as ProvisionFailed (ADR-0014), never
// a crash-looping pod with no explanation.
var minStorageSize = resource.MustParse("1Gi")

// dns1035Label is the name shape that is simultaneously a valid Service and
// StatefulSet name AND, with hyphens mapped to underscores, a valid unquoted
// SQL identifier: starts with a letter, no dots, ≤63 chars. The Postgres object
// name is a DNS-1123 subdomain (looser: leading digits, dots, ≤253), so a name
// the schema accepts can still fail here — by design, validation lives in the
// reconciler, not a webhook (ADR-0010).
var dns1035Label = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// PostgresReconciler renders the v1 service catalog: a digest-pinned Postgres
// StatefulSet, a headless Service, and the connection Secret named exactly
// metadata.name in orkano-apps (ADR-0014). The Secret value lives nowhere else
// (INV-03); Apps reference it by name. Cascade on delete rides ownerReference
// GC and the StatefulSet's PVC retention policy, so there is no finalizer.
type PostgresReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader is the uncached client used for the connection Secret and the
	// data PVC: the operator holds no secrets/PVC list+watch (INV: it cannot
	// enumerate Secrets), so those types are absent from the manager cache and
	// must be read direct from the apiserver, mirroring githubapp.TokenSource.
	APIReader client.Reader
}

func (r *PostgresReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pg orkanov1alpha1.Postgres
	if err := r.Get(ctx, req.NamespacedName, &pg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Children carry ownerReferences and the StatefulSet's PVC retention policy
	// deletes the data volume, so deletion needs no finalizer work.
	if !pg.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	statusBefore := pg.Status.DeepCopy()

	// SQL-safe identifier (and Service/StatefulSet name) derived from the object
	// name: "api-db" -> "api_db". A name the schema accepts but that cannot yield
	// a valid identifier is a permanent, user-visible ProvisionFailed. The ≤63
	// cap is enforced separately from the regex: a longer name would pass the
	// character class but PostgreSQL truncates identifiers at NAMEDATALEN-1 (63),
	// silently diverging the real DB/role from what the connection Secret says.
	if !dns1035Label.MatchString(pg.Name) || len(pg.Name) > maxIdentifierLen {
		return ctrl.Result{}, r.failReady(ctx, &pg, statusBefore, reasonProvisionFailed, fmt.Errorf(
			"name %q must be a valid DNS-1035 label (start with a letter, no dots, ≤%d chars) to back a Service and a SQL identifier", pg.Name, maxIdentifierLen))
	}
	ident := strings.ReplaceAll(pg.Name, "-", "_")

	image, ok := postgresImages[postgresVersion(&pg)]
	if !ok {
		return ctrl.Result{}, r.failReady(ctx, &pg, statusBefore, reasonProvisionFailed, fmt.Errorf(
			"no image pinned for PostgreSQL version %q", postgresVersion(&pg)))
	}

	storage := postgresStorageSize(&pg)
	if storage.Cmp(minStorageSize) < 0 {
		return ctrl.Result{}, r.failReady(ctx, &pg, statusBefore, reasonProvisionFailed, fmt.Errorf(
			"storageSize %s is below the %s minimum a database needs to start", storage.String(), minStorageSize.String()))
	}

	// Infrastructure errors below take reasonReconcileError, NOT ProvisionFailed:
	// a transient apiserver blip must not look like the permanent, user-actionable
	// failures above — a user who reads ProvisionFailed might delete-and-recreate,
	// destroying the database (mirrors the App controller's reason split).
	if err := r.ensureSecret(ctx, &pg, ident); err != nil {
		return ctrl.Result{}, r.failReady(ctx, &pg, statusBefore, reasonReconcileError, fmt.Errorf("reconciling connection Secret: %w", err))
	}
	pg.Status.SecretName = pg.Name

	if err := r.ensureService(ctx, &pg); err != nil {
		return ctrl.Result{}, r.failReady(ctx, &pg, statusBefore, reasonReconcileError, fmt.Errorf("reconciling Service: %w", err))
	}

	sts, err := r.ensureStatefulSet(ctx, &pg, ident, image, storage)
	if err != nil {
		return ctrl.Result{}, r.failReady(ctx, &pg, statusBefore, reasonReconcileError, fmt.Errorf("reconciling StatefulSet: %w", err))
	}

	// Storage is grow-only, enforced here (not the schema) per native PVC
	// semantics: the apiserver accepts a shrink, only the controller rejects it.
	shrunk, err := r.reconcileStorage(ctx, &pg, storage)
	if err != nil {
		return ctrl.Result{}, r.failReady(ctx, &pg, statusBefore, reasonReconcileError, fmt.Errorf("reconciling storage: %w", err))
	}
	if shrunk != nil {
		// A shrink is a permanent, user-actionable request, not an infra error.
		return ctrl.Result{}, r.failReady(ctx, &pg, statusBefore, reasonProvisionFailed, fmt.Errorf(
			"cannot shrink storageSize: %s requested, %s already provisioned (PVC expansion is one-way)", storage.String(), shrunk.String()))
	}

	if sts.Status.ReadyReplicas >= 1 {
		setPostgresReady(&pg, metav1.ConditionTrue, reasonAvailable, "database is accepting connections")
	} else {
		setPostgresReady(&pg, metav1.ConditionFalse, reasonProvisioning, "waiting for the database pod to become ready")
	}
	log.V(1).Info("reconciled Postgres", "ready", sts.Status.ReadyReplicas)
	return ctrl.Result{}, r.updateStatus(ctx, &pg, statusBefore)
}

// ensureSecret writes the connection Secret, generating the password once and
// preserving it across reconciles — regenerating it would lock out the running
// database (the password is baked into the data directory at initdb) and every
// connected App. The Secret is owned by the Postgres object so deletion
// cascades; INV-03: no value here ever appears in a CR.
func (r *PostgresReconciler) ensureSecret(ctx context.Context, pg *orkanov1alpha1.Postgres, ident string) error {
	key := types.NamespacedName{Namespace: pg.Namespace, Name: pg.Name}
	existing := &corev1.Secret{}
	err := r.APIReader.Get(ctx, key, existing)
	notFound := apierrors.IsNotFound(err)
	if err != nil && !notFound {
		return err
	}

	// Snapshot the STORED state before mutating in place: ensureSecret reuses the
	// fetched object as the update target, and SetControllerReference / the field
	// writes below alter it, so comparing the mutated object to itself would
	// always look unchanged and silently skip persisting a missing ownerRef or
	// label (a recoverable orphan with live credentials).
	password := ""
	var (
		storedData  map[string][]byte
		storedType  corev1.SecretType
		storedOwned bool
		storedLabel string
	)
	if !notFound {
		password = string(existing.Data[orkanov1alpha1.SecretKeyPassword])
		storedData = existing.Data
		storedType = existing.Type
		storedOwned = metav1.IsControlledBy(existing, pg)
		storedLabel = existing.Labels[managedByLabel]
	}
	if password == "" {
		password, err = generatePassword()
		if err != nil {
			return err
		}
	}

	host := fmt.Sprintf("%s.%s.svc.cluster.local", pg.Name, pg.Namespace)
	data := map[string][]byte{
		orkanov1alpha1.SecretKeyURI: []byte(fmt.Sprintf(
			"postgresql://%s:%s@%s:%d/%s", ident, password, host, postgresPort, ident)),
		orkanov1alpha1.SecretKeyHost:     []byte(host),
		orkanov1alpha1.SecretKeyPort:     []byte(fmt.Sprint(postgresPort)),
		orkanov1alpha1.SecretKeyDatabase: []byte(ident),
		orkanov1alpha1.SecretKeyUsername: []byte(ident),
		orkanov1alpha1.SecretKeyPassword: []byte(password),
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pg.Name, Namespace: pg.Namespace}}
	if !notFound {
		secret = existing
	}
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[managedByLabel] = managedByValue
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = data
	if err := controllerutil.SetControllerReference(pg, secret, r.Scheme); err != nil {
		return err
	}

	if notFound {
		return r.Create(ctx, secret)
	}
	// Avoid a no-op Update each reconcile (the password is preserved, so the
	// steady state compares equal) while still healing ownerRef/label/type drift.
	if equality.Semantic.DeepEqual(storedData, data) &&
		storedType == corev1.SecretTypeOpaque &&
		storedOwned &&
		storedLabel == managedByValue {
		return nil
	}
	return r.Update(ctx, secret)
}

func (r *PostgresReconciler) ensureService(ctx context.Context, pg *orkanov1alpha1.Postgres) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: pg.Name, Namespace: pg.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if !svc.CreationTimestamp.IsZero() && metav1.GetControllerOf(svc) == nil {
			return fmt.Errorf("existing Service %s/%s is not managed by Orkano; rename the Postgres or delete the Service", svc.Namespace, svc.Name)
		}
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels[managedByLabel] = managedByValue
		// Headless: a StatefulSet wants stable per-pod DNS, and the single
		// replica's address resolves <name>.<ns>.svc.cluster.local for clients.
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = map[string]string{postgresLabel: pg.Name}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       postgresPortName,
			Port:       postgresPort,
			TargetPort: intstr.FromString(postgresPortName),
			Protocol:   corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(pg, svc, r.Scheme)
	})
	return err
}

func (r *PostgresReconciler) ensureStatefulSet(ctx context.Context, pg *orkanov1alpha1.Postgres, ident, image string, storage resource.Quantity) (*appsv1.StatefulSet, error) {
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: pg.Name, Namespace: pg.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		if !sts.CreationTimestamp.IsZero() && metav1.GetControllerOf(sts) == nil {
			return fmt.Errorf("existing StatefulSet %s/%s is not managed by Orkano; rename the Postgres or delete the StatefulSet", sts.Namespace, sts.Name)
		}
		if sts.Labels == nil {
			sts.Labels = map[string]string{}
		}
		sts.Labels[managedByLabel] = managedByValue

		// Immutable fields, set once at creation: the selector, the headless
		// Service name, and the volumeClaimTemplate (whose storage is grown by
		// patching the live PVC, not by editing this immutable template).
		if sts.CreationTimestamp.IsZero() {
			sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{postgresLabel: pg.Name}}
			sts.Spec.ServiceName = pg.Name
			sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: postgresDataVolume},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: storage},
					},
				},
			}}
		}

		sts.Spec.Replicas = ptr.To(int32(1))
		// Delete the data PVC with the set: a Postgres object is the single
		// owner of its data, and ADR-0014's delete-and-recreate upgrade story
		// already treats deletion as data loss. Without this, recreating a
		// same-named object would rebind the old PVC under a fresh password and
		// fail to authenticate.
		sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		}
		mutatePostgresPodTemplate(pg, &sts.Spec.Template, ident, image)
		return controllerutil.SetControllerReference(pg, sts, r.Scheme)
	})
	return sts, err
}

// mutatePostgresPodTemplate sets only the fields Orkano owns, carrying the
// apiserver's probe defaults explicitly so a freshly rendered template compares
// equal to the stored one (idempotent re-reconcile, like the App controller).
func mutatePostgresPodTemplate(pg *orkanov1alpha1.Postgres, tmpl *corev1.PodTemplateSpec, ident, image string) {
	tmpl.Labels = map[string]string{postgresLabel: pg.Name, managedByLabel: managedByValue}

	tmpl.Spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		RunAsUser:      ptr.To(postgresUID),
		RunAsGroup:     ptr.To(postgresUID),
		FSGroup:        ptr.To(postgresUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}

	if len(tmpl.Spec.Containers) != 1 || tmpl.Spec.Containers[0].Name != postgresContainerName {
		tmpl.Spec.Containers = []corev1.Container{{Name: postgresContainerName}}
	}
	c := &tmpl.Spec.Containers[0]
	c.Image = image
	c.Ports = []corev1.ContainerPort{{Name: postgresPortName, ContainerPort: postgresPort, Protocol: corev1.ProtocolTCP}}
	// Credentials come from the connection Secret by reference (INV-03): no
	// value is written into the workload spec. POSTGRES_DB/USER seed initdb with
	// the SQL identifier; PGDATA is the 0700 subdirectory of the mount.
	c.Env = []corev1.EnvVar{
		secretEnv("POSTGRES_USER", pg.Name, orkanov1alpha1.SecretKeyUsername),
		secretEnv("POSTGRES_PASSWORD", pg.Name, orkanov1alpha1.SecretKeyPassword),
		secretEnv("POSTGRES_DB", pg.Name, orkanov1alpha1.SecretKeyDatabase),
		{Name: "PGDATA", Value: postgresPGData},
	}
	c.VolumeMounts = []corev1.VolumeMount{{Name: postgresDataVolume, MountPath: postgresDataMount}}
	c.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		// Memory limit above request (burstable): a DB's working set varies with
		// connections, so request==limit would OOM under light load. No CPU
		// limit, matching the App workload policy.
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	// The official image binds TCP only AFTER initdb finishes (the first-init
	// temp server listens on the Unix socket alone), and initdb can run minutes
	// on slow storage. A startup probe gives it that window; the liveness probe
	// does not fire until startup succeeds, so a cold start can never trip it
	// into a crash loop. pg_isready is the real readiness signal; TCP liveness is
	// gentler than re-execing pg_isready under load.
	c.StartupProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"pg_isready", "-U", ident, "-d", ident},
		}},
		PeriodSeconds:    5,
		TimeoutSeconds:   3,
		SuccessThreshold: 1,
		FailureThreshold: 60, // ~5 min for initdb before liveness/readiness apply
	}
	c.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"pg_isready", "-U", ident, "-d", ident},
		}},
		PeriodSeconds:    10,
		TimeoutSeconds:   3,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
	c.LivenessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(postgresPortName)}},
		PeriodSeconds:    20,
		TimeoutSeconds:   3,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
	// readOnlyRootFilesystem is deliberately left unset: the official postgres
	// image writes to /tmp and /var/run/postgresql at runtime, so locking the
	// root FS needs tmpfs mounts whose exact set can only be verified by booting
	// the image — full restricted-PSA hardening rides M1.6's live E2E. The pod
	// is admitted today (orkano-apps enforces baseline; restricted is warn-only).
	c.SecurityContext = &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func secretEnv(name, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			Key:                  key,
		},
	}}
}

// reconcileStorage grows the data PVC when storageSize increases and reports a
// shrink (returns the already-provisioned size) so the caller can surface
// ProvisionFailed. The PVC is read uncached (it is not in the manager cache).
// A not-yet-created PVC means the set is still provisioning: nothing to do.
func (r *PostgresReconciler) reconcileStorage(ctx context.Context, pg *orkanov1alpha1.Postgres, desired resource.Quantity) (*resource.Quantity, error) {
	key := types.NamespacedName{Namespace: pg.Namespace, Name: postgresDataVolume + "-" + pg.Name + "-0"}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.APIReader.Get(ctx, key, pvc); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	switch desired.Cmp(current) {
	case 0:
		return nil, nil
	case -1:
		return &current, nil
	default:
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = desired
		if err := r.Update(ctx, pvc); err != nil {
			return nil, fmt.Errorf("growing data PVC to %s: %w", desired.String(), err)
		}
		logf.FromContext(ctx).Info("grew Postgres data volume", "from", current.String(), "to", desired.String())
		return nil, nil
	}
}

func postgresVersion(pg *orkanov1alpha1.Postgres) string {
	if pg.Spec.Version == "" {
		return "16"
	}
	return pg.Spec.Version
}

func postgresStorageSize(pg *orkanov1alpha1.Postgres) resource.Quantity {
	if pg.Spec.StorageSize == nil {
		return defaultStorageSize
	}
	return *pg.Spec.StorageSize
}

// generatePassword returns a URL-safe (hex) password — no characters that need
// percent-encoding in the connection URI.
func generatePassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func setPostgresReady(pg *orkanov1alpha1.Postgres, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&pg.Status.Conditions, metav1.Condition{
		Type:               orkanov1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: pg.Generation,
	})
}

// failReady records why reconciliation stopped (ProvisionFailed for permanent,
// user-actionable refusals; ReconcileError for transient infrastructure errors)
// before returning the error, so the cause is visible in `kubectl describe`,
// never just a silent reconcile loop. Conflicts heal on the immediate retry and
// would only flap the condition.
func (r *PostgresReconciler) failReady(ctx context.Context, pg *orkanov1alpha1.Postgres, before *orkanov1alpha1.PostgresStatus, reason string, err error) error {
	if apierrors.IsConflict(err) {
		return err
	}
	setPostgresReady(pg, metav1.ConditionFalse, reason, err.Error())
	if statusErr := r.updateStatus(ctx, pg, before); statusErr != nil {
		logf.FromContext(ctx).Error(statusErr, "failed to record failure condition", "reason", reason)
	}
	return err
}

// updateStatus writes status only when it changed, so reconciles triggered by
// our own status writes settle instead of looping.
func (r *PostgresReconciler) updateStatus(ctx context.Context, pg *orkanov1alpha1.Postgres, before *orkanov1alpha1.PostgresStatus) error {
	pg.Status.ObservedGeneration = pg.Generation
	if equality.Semantic.DeepEqual(before, &pg.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, pg); err != nil {
		return fmt.Errorf("updating Postgres status: %w", err)
	}
	return nil
}

func (r *PostgresReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orkanov1alpha1.Postgres{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		// The connection Secret is owned (GC cascades) but not watched: the
		// operator holds no secrets list/watch, so it is absent from the cache.
		Named("postgres").
		Complete(r)
}
