package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	mongoLabel                     = "app.orkano.io/mongo"
	mongoContainerName             = "mongo"
	mongoPort                      = int32(27017)
	mongoPortName                  = "mongo"
	mongoDataVolume                = "data"
	mongoDataMount                 = "/data/db"
	mongoUID                       = int64(999)
	mongoExpressLabel              = "app.orkano.io/mongo-express"
	mongoExpressContainerName      = "mongo-express"
	mongoExpressSuffix             = "-mongo-express"
	mongoExpressPort               = int32(8081)
	mongoExpressPortName           = "http"
	mongoExpressUID                = int64(1000)
	mongoExpressCookieSecretKey    = "cookieSecret"
	mongoExpressSessionSecretKey   = "sessionSecret"
	reasonMongoExpressDisabled     = "Disabled"
	reasonMongoExpressProvisioning = "Provisioning"
	reasonMongoExpressAvailable    = "Available"
)

var mongoImages = map[string]string{
	"8.0": "mongo:8.0@sha256:3ce3de7f40e914034b03b7dec654005ab54f7dc8306937e44ec6760d9e9409a1",
}

const mongoExpressImage = "mongo-express:1.0.2-20-alpine3.19@sha256:1aae0077525133249133d42980ba23998712a1077e02ac0ac295b50a7a79d550"

// MongoReconciler renders one authenticated MongoDB StatefulSet, headless
// Service, and connection Secret. Secret values never leave the Secret
// (INV-03); Apps consume its uri key as MONGODB_URI.
type MongoReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	APIReader client.Reader
}

func (r *MongoReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var mongo orkanov1alpha1.Mongo
	if err := r.Get(ctx, req.NamespacedName, &mongo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !mongo.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	statusBefore := mongo.Status.DeepCopy()
	if !dns1035Label.MatchString(mongo.Name) || len(mongo.Name) > maxIdentifierLen {
		return ctrl.Result{}, r.failReady(ctx, &mongo, statusBefore, reasonProvisionFailed, fmt.Errorf(
			"name %q must be a valid DNS-1035 label (start with a letter, no dots, ≤%d chars) to back a Service and MongoDB database", mongo.Name, maxIdentifierLen))
	}
	ident := strings.ReplaceAll(mongo.Name, "-", "_")

	image, ok := mongoImages[mongoVersion(&mongo)]
	if !ok {
		return ctrl.Result{}, r.failReady(ctx, &mongo, statusBefore, reasonProvisionFailed, fmt.Errorf(
			"no image pinned for MongoDB version %q", mongoVersion(&mongo)))
	}

	storage := mongoStorageSize(&mongo)
	if storage.Cmp(minStorageSize) < 0 {
		return ctrl.Result{}, r.failReady(ctx, &mongo, statusBefore, reasonProvisionFailed, fmt.Errorf(
			"storageSize %s is below the %s minimum a database needs to start", storage.String(), minStorageSize.String()))
	}

	if !mongo.MongoExpressEnabled() {
		if err := r.disableMongoExpress(ctx, &mongo); err != nil {
			setMongoExpressReady(&mongo, metav1.ConditionFalse, reasonReconcileError, err.Error())
			if statusErr := r.updateStatus(ctx, &mongo, statusBefore); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, fmt.Errorf("disabling Mongo Express: %w", err)
		}
		mongo.Status.MongoExpressServiceName = ""
		setMongoExpressReady(&mongo, metav1.ConditionFalse, reasonMongoExpressDisabled, "Mongo Express is disabled")
	}

	if err := r.ensureSecret(ctx, &mongo, ident); err != nil {
		var owned *controllerutil.AlreadyOwnedError
		if errors.As(err, &owned) {
			return ctrl.Result{}, r.failReady(ctx, &mongo, statusBefore, reasonProvisionFailed, fmt.Errorf(
				"connection Secret %q already exists and is owned by %s %s — pick a different name", mongo.Name, owned.Owner.Kind, owned.Owner.Name))
		}
		return ctrl.Result{}, r.failReady(ctx, &mongo, statusBefore, reasonReconcileError, fmt.Errorf("reconciling connection Secret: %w", err))
	}
	mongo.Status.SecretName = mongo.Name

	if err := r.ensureService(ctx, &mongo); err != nil {
		return ctrl.Result{}, r.failReady(ctx, &mongo, statusBefore, reasonReconcileError, fmt.Errorf("reconciling Service: %w", err))
	}
	sts, err := r.ensureStatefulSet(ctx, &mongo, image, storage)
	if err != nil {
		return ctrl.Result{}, r.failReady(ctx, &mongo, statusBefore, reasonReconcileError, fmt.Errorf("reconciling StatefulSet: %w", err))
	}

	shrunk, err := r.reconcileStorage(ctx, &mongo, storage)
	if err != nil {
		return ctrl.Result{}, r.failReady(ctx, &mongo, statusBefore, reasonReconcileError, fmt.Errorf("reconciling storage: %w", err))
	}
	if shrunk != nil {
		return ctrl.Result{}, r.failReady(ctx, &mongo, statusBefore, reasonProvisionFailed, fmt.Errorf(
			"cannot shrink storageSize: %s requested, %s already provisioned (PVC expansion is one-way)", storage.String(), shrunk.String()))
	}

	if sts.Status.ReadyReplicas >= 1 {
		setMongoReady(&mongo, metav1.ConditionTrue, reasonAvailable, "database is accepting connections")
	} else {
		setMongoReady(&mongo, metav1.ConditionFalse, reasonProvisioning, "waiting for the database pod to become ready")
	}
	if mongo.MongoExpressEnabled() {
		if err := r.reconcileMongoExpress(ctx, &mongo); err != nil {
			setMongoExpressReady(&mongo, metav1.ConditionFalse, reasonReconcileError, err.Error())
			if statusErr := r.updateStatus(ctx, &mongo, statusBefore); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, fmt.Errorf("reconciling Mongo Express: %w", err)
		}
	}
	logf.FromContext(ctx).V(1).Info("reconciled Mongo", "ready", sts.Status.ReadyReplicas)
	return ctrl.Result{}, r.updateStatus(ctx, &mongo, statusBefore)
}

func (r *MongoReconciler) ensureSecret(ctx context.Context, mongo *orkanov1alpha1.Mongo, ident string) error {
	key := types.NamespacedName{Namespace: mongo.Namespace, Name: mongo.Name}
	existing := &corev1.Secret{}
	err := r.APIReader.Get(ctx, key, existing)
	notFound := apierrors.IsNotFound(err)
	if err != nil && !notFound {
		return err
	}

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
		storedOwned = metav1.IsControlledBy(existing, mongo)
		storedLabel = existing.Labels[managedByLabel]
	}
	if password == "" {
		password, err = generatePassword()
		if err != nil {
			return err
		}
	}

	host := fmt.Sprintf("%s.%s.svc.cluster.local", mongo.Name, mongo.Namespace)
	data := map[string][]byte{
		orkanov1alpha1.SecretKeyURI: []byte(fmt.Sprintf(
			"mongodb://%s:%s@%s:%d/%s?authSource=admin", ident, password, host, mongoPort, ident)),
		orkanov1alpha1.SecretKeyHost:     []byte(host),
		orkanov1alpha1.SecretKeyPort:     []byte(fmt.Sprint(mongoPort)),
		orkanov1alpha1.SecretKeyDatabase: []byte(ident),
		orkanov1alpha1.SecretKeyUsername: []byte(ident),
		orkanov1alpha1.SecretKeyPassword: []byte(password),
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: mongo.Name, Namespace: mongo.Namespace}}
	if !notFound {
		secret = existing
	}
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[managedByLabel] = managedByValue
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = data
	if err := controllerutil.SetControllerReference(mongo, secret, r.Scheme); err != nil {
		return err
	}

	if notFound {
		return r.Create(ctx, secret)
	}
	if equality.Semantic.DeepEqual(storedData, data) &&
		storedType == corev1.SecretTypeOpaque && storedOwned && storedLabel == managedByValue {
		return nil
	}
	return r.Update(ctx, secret)
}

func (r *MongoReconciler) ensureService(ctx context.Context, mongo *orkanov1alpha1.Mongo) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: mongo.Name, Namespace: mongo.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if !svc.CreationTimestamp.IsZero() && metav1.GetControllerOf(svc) == nil {
			return fmt.Errorf("existing Service %s/%s is not managed by Orkano; rename the Mongo or delete the Service", svc.Namespace, svc.Name)
		}
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels[managedByLabel] = managedByValue
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = map[string]string{mongoLabel: mongo.Name}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name: mongoPortName, Port: mongoPort, TargetPort: intstr.FromString(mongoPortName), Protocol: corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(mongo, svc, r.Scheme)
	})
	return err
}

func (r *MongoReconciler) ensureStatefulSet(ctx context.Context, mongo *orkanov1alpha1.Mongo, image string, storage resource.Quantity) (*appsv1.StatefulSet, error) {
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: mongo.Name, Namespace: mongo.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		if !sts.CreationTimestamp.IsZero() && metav1.GetControllerOf(sts) == nil {
			return fmt.Errorf("existing StatefulSet %s/%s is not managed by Orkano; rename the Mongo or delete the StatefulSet", sts.Namespace, sts.Name)
		}
		if sts.Labels == nil {
			sts.Labels = map[string]string{}
		}
		sts.Labels[managedByLabel] = managedByValue
		if sts.CreationTimestamp.IsZero() {
			sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{mongoLabel: mongo.Name}}
			sts.Spec.ServiceName = mongo.Name
			sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: mongoDataVolume},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storage}},
				},
			}}
		}
		sts.Spec.Replicas = ptr.To(int32(1))
		sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		}
		mutateMongoPodTemplate(mongo, &sts.Spec.Template, image)
		return controllerutil.SetControllerReference(mongo, sts, r.Scheme)
	})
	return sts, err
}

func mutateMongoPodTemplate(mongo *orkanov1alpha1.Mongo, tmpl *corev1.PodTemplateSpec, image string) {
	tmpl.Labels = map[string]string{mongoLabel: mongo.Name, managedByLabel: managedByValue}
	tmpl.Spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(mongoUID), RunAsGroup: ptr.To(mongoUID), FSGroup: ptr.To(mongoUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if len(tmpl.Spec.Containers) != 1 || tmpl.Spec.Containers[0].Name != mongoContainerName {
		tmpl.Spec.Containers = []corev1.Container{{Name: mongoContainerName}}
	}
	c := &tmpl.Spec.Containers[0]
	c.Image = image
	c.Ports = []corev1.ContainerPort{{Name: mongoPortName, ContainerPort: mongoPort, Protocol: corev1.ProtocolTCP}}
	c.Env = []corev1.EnvVar{
		secretEnv("MONGO_INITDB_ROOT_USERNAME", mongo.Name, orkanov1alpha1.SecretKeyUsername),
		secretEnv("MONGO_INITDB_ROOT_PASSWORD", mongo.Name, orkanov1alpha1.SecretKeyPassword),
		secretEnv("MONGO_INITDB_DATABASE", mongo.Name, orkanov1alpha1.SecretKeyDatabase),
	}
	c.VolumeMounts = []corev1.VolumeMount{{Name: mongoDataVolume, MountPath: mongoDataMount}}
	c.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	tcpProbe := func(period, timeout, failure int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(mongoPortName)}},
			PeriodSeconds: period, TimeoutSeconds: timeout, SuccessThreshold: 1, FailureThreshold: failure,
		}
	}
	c.StartupProbe = tcpProbe(5, 3, 60)
	c.ReadinessProbe = tcpProbe(10, 3, 3)
	c.LivenessProbe = tcpProbe(20, 3, 3)
	c.SecurityContext = &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func mongoExpressResourceName(mongoName string) string {
	if len(mongoName)+len(mongoExpressSuffix) <= 63 {
		return mongoName + mongoExpressSuffix
	}
	sum := sha256.Sum256([]byte(mongoName))
	hash := hex.EncodeToString(sum[:4])
	maxPrefix := 63 - len(mongoExpressSuffix) - len(hash) - 1
	prefix := strings.TrimRight(mongoName[:maxPrefix], "-")
	return prefix + "-" + hash + mongoExpressSuffix
}

func mongoExpressBasePath(mongoName string) string {
	return "/api/mongo/" + mongoName + "/express/"
}

func (r *MongoReconciler) reconcileMongoExpress(ctx context.Context, mongo *orkanov1alpha1.Mongo) error {
	name := mongoExpressResourceName(mongo.Name)
	mongo.Status.MongoExpressServiceName = name
	if err := r.ensureMongoExpressSecret(ctx, mongo, name); err != nil {
		return fmt.Errorf("session Secret: %w", err)
	}
	if err := r.ensureMongoExpressService(ctx, mongo, name); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	deployment, err := r.ensureMongoExpressDeployment(ctx, mongo, name)
	if err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	if err := r.ensureMongoExpressNetworkPolicy(ctx, mongo, name); err != nil {
		return fmt.Errorf("network policy: %w", err)
	}
	if deployment.Status.AvailableReplicas >= 1 {
		setMongoExpressReady(mongo, metav1.ConditionTrue, reasonMongoExpressAvailable, "Mongo Express is available through the authenticated dashboard")
	} else {
		setMongoExpressReady(mongo, metav1.ConditionFalse, reasonMongoExpressProvisioning, "waiting for Mongo Express to become ready")
	}
	return nil
}

func (r *MongoReconciler) ensureMongoExpressSecret(ctx context.Context, mongo *orkanov1alpha1.Mongo, name string) error {
	key := types.NamespacedName{Namespace: mongo.Namespace, Name: name}
	secret := &corev1.Secret{}
	err := r.APIReader.Get(ctx, key, secret)
	notFound := apierrors.IsNotFound(err)
	if err != nil && !notFound {
		return err
	}
	if notFound {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mongo.Namespace}}
	} else if !metav1.IsControlledBy(secret, mongo) {
		return ownedObjectError("Secret", secret, mongo)
	}
	before := secret.DeepCopy()
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[managedByLabel] = managedByValue
	secret.Labels[mongoExpressLabel] = mongo.Name
	secret.Type = corev1.SecretTypeOpaque
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	for _, key := range []string{mongoExpressCookieSecretKey, mongoExpressSessionSecretKey} {
		if len(secret.Data[key]) != 0 {
			continue
		}
		value, generateErr := generatePassword()
		if generateErr != nil {
			return generateErr
		}
		secret.Data[key] = []byte(value)
	}
	if err := controllerutil.SetControllerReference(mongo, secret, r.Scheme); err != nil {
		return err
	}
	if notFound {
		return r.Create(ctx, secret)
	}
	if equality.Semantic.DeepEqual(before, secret) {
		return nil
	}
	return r.Update(ctx, secret)
}

func (r *MongoReconciler) ensureMongoExpressService(ctx context.Context, mongo *orkanov1alpha1.Mongo, name string) error {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mongo.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if !service.CreationTimestamp.IsZero() && !metav1.IsControlledBy(service, mongo) {
			return ownedObjectError("Service", service, mongo)
		}
		service.Labels = mongoExpressLabels(mongo)
		service.Spec.Selector = map[string]string{mongoExpressLabel: mongo.Name}
		service.Spec.Ports = []corev1.ServicePort{{
			Name: mongoExpressPortName, Port: mongoExpressPort,
			TargetPort: intstr.FromString(mongoExpressPortName), Protocol: corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(mongo, service, r.Scheme)
	})
	return err
}

func (r *MongoReconciler) ensureMongoExpressDeployment(ctx context.Context, mongo *orkanov1alpha1.Mongo, name string) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mongo.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		if !deployment.CreationTimestamp.IsZero() && !metav1.IsControlledBy(deployment, mongo) {
			return ownedObjectError("Deployment", deployment, mongo)
		}
		deployment.Labels = mongoExpressLabels(mongo)
		if deployment.CreationTimestamp.IsZero() {
			deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{mongoExpressLabel: mongo.Name}}
		}
		deployment.Spec.Replicas = ptr.To(int32(1))
		mutateMongoExpressPodTemplate(mongo, name, &deployment.Spec.Template)
		return controllerutil.SetControllerReference(mongo, deployment, r.Scheme)
	})
	return deployment, err
}

func mutateMongoExpressPodTemplate(mongo *orkanov1alpha1.Mongo, secretName string, tmpl *corev1.PodTemplateSpec) {
	tmpl.Labels = mongoExpressLabels(mongo)
	tmpl.Spec.AutomountServiceAccountToken = ptr.To(false)
	tmpl.Spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(mongoExpressUID), RunAsGroup: ptr.To(mongoExpressUID), FSGroup: ptr.To(mongoExpressUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	tmpl.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	if len(tmpl.Spec.Containers) != 1 || tmpl.Spec.Containers[0].Name != mongoExpressContainerName {
		tmpl.Spec.Containers = []corev1.Container{{Name: mongoExpressContainerName}}
	}
	c := &tmpl.Spec.Containers[0]
	c.Image = mongoExpressImage
	c.Ports = []corev1.ContainerPort{{Name: mongoExpressPortName, ContainerPort: mongoExpressPort, Protocol: corev1.ProtocolTCP}}
	c.Env = []corev1.EnvVar{
		secretEnv("ME_CONFIG_MONGODB_URL", mongo.Name, orkanov1alpha1.SecretKeyURI),
		{Name: "ME_CONFIG_MONGODB_ENABLE_ADMIN", Value: "true"},
		{Name: "ME_CONFIG_BASICAUTH", Value: "false"},
		{Name: "ME_CONFIG_SITE_BASEURL", Value: mongoExpressBasePath(mongo.Name)},
		{Name: "ME_CONFIG_HEALTH_CHECK_PATH", Value: "/status"},
		{Name: "ME_CONFIG_OPTIONS_CONFIRM_DELETE", Value: "true"},
		{Name: "VCAP_APP_HOST", Value: "0.0.0.0"},
		secretEnv("ME_CONFIG_SITE_COOKIESECRET", secretName, mongoExpressCookieSecretKey),
		secretEnv("ME_CONFIG_SITE_SESSIONSECRET", secretName, mongoExpressSessionSecretKey),
	}
	c.VolumeMounts = []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	c.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	healthPath := mongoExpressBasePath(mongo.Name) + "status"
	httpProbe := func(period, timeout, failure int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: healthPath, Port: intstr.FromString(mongoExpressPortName)}},
			PeriodSeconds: period, TimeoutSeconds: timeout, SuccessThreshold: 1, FailureThreshold: failure,
		}
	}
	c.StartupProbe = httpProbe(5, 3, 60)
	c.ReadinessProbe = httpProbe(10, 3, 3)
	c.LivenessProbe = httpProbe(20, 3, 3)
	c.SecurityContext = &corev1.SecurityContext{
		RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(mongoExpressUID), RunAsGroup: ptr.To(mongoExpressUID),
		ReadOnlyRootFilesystem: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func (r *MongoReconciler) ensureMongoExpressNetworkPolicy(ctx context.Context, mongo *orkanov1alpha1.Mongo, name string) error {
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mongo.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		if !policy.CreationTimestamp.IsZero() && !metav1.IsControlledBy(policy, mongo) {
			return ownedObjectError("NetworkPolicy", policy, mongo)
		}
		policy.Labels = mongoExpressLabels(mongo)
		policy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{mongoExpressLabel: mongo.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": systemNamespace}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "orkano-dashboard"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(mongoExpressPort))}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{mongoLabel: mongo.Name}}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(mongoPort))}},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: ptr.To(corev1.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))},
						{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt32(53))},
					},
				},
			},
		}
		return controllerutil.SetControllerReference(mongo, policy, r.Scheme)
	})
	return err
}

func mongoExpressLabels(mongo *orkanov1alpha1.Mongo) map[string]string {
	return map[string]string{
		mongoExpressLabel:            mongo.Name,
		managedByLabel:               managedByValue,
		"app.kubernetes.io/name":     mongoExpressContainerName,
		"app.kubernetes.io/instance": mongoExpressResourceName(mongo.Name),
	}
}

func ownedObjectError(kind string, obj metav1.Object, mongo *orkanov1alpha1.Mongo) error {
	owner := metav1.GetControllerOf(obj)
	if owner == nil {
		return fmt.Errorf("existing %s %s/%s is not managed by Orkano; delete it or disable Mongo Express", kind, obj.GetNamespace(), obj.GetName())
	}
	return fmt.Errorf("existing %s %s/%s is owned by %s %s; delete it or disable Mongo Express", kind, obj.GetNamespace(), obj.GetName(), owner.Kind, owner.Name)
}

func (r *MongoReconciler) disableMongoExpress(ctx context.Context, mongo *orkanov1alpha1.Mongo) error {
	name := mongoExpressResourceName(mongo.Name)
	objects := []client.Object{
		&appsv1.Deployment{},
		&corev1.Service{},
		&networkingv1.NetworkPolicy{},
	}
	for _, obj := range objects {
		if err := r.deleteOwnedMongoExpressObject(ctx, mongo, name, obj, r.Client); err != nil {
			return err
		}
	}
	return r.deleteOwnedMongoExpressObject(ctx, mongo, name, &corev1.Secret{}, r.APIReader)
}

func (r *MongoReconciler) deleteOwnedMongoExpressObject(ctx context.Context, mongo *orkanov1alpha1.Mongo, name string, obj client.Object, reader client.Reader) error {
	key := types.NamespacedName{Name: name, Namespace: mongo.Namespace}
	if err := reader.Get(ctx, key, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, mongo) {
		return nil
	}
	if err := r.Delete(ctx, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

func (r *MongoReconciler) reconcileStorage(ctx context.Context, mongo *orkanov1alpha1.Mongo, desired resource.Quantity) (*resource.Quantity, error) {
	key := types.NamespacedName{Namespace: mongo.Namespace, Name: mongoDataVolume + "-" + mongo.Name + "-0"}
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
		logf.FromContext(ctx).Info("grew MongoDB data volume", "from", current.String(), "to", desired.String())
		return nil, nil
	}
}

func mongoVersion(mongo *orkanov1alpha1.Mongo) string {
	if mongo.Spec.Version == "" {
		return "8.0"
	}
	return mongo.Spec.Version
}

func mongoStorageSize(mongo *orkanov1alpha1.Mongo) resource.Quantity {
	if mongo.Spec.StorageSize == nil {
		return defaultStorageSize
	}
	return *mongo.Spec.StorageSize
}

func setMongoReady(mongo *orkanov1alpha1.Mongo, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&mongo.Status.Conditions, metav1.Condition{
		Type: orkanov1alpha1.ConditionReady, Status: status, Reason: reason, Message: message, ObservedGeneration: mongo.Generation,
	})
}

func setMongoExpressReady(mongo *orkanov1alpha1.Mongo, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&mongo.Status.Conditions, metav1.Condition{
		Type: orkanov1alpha1.ConditionMongoExpressReady, Status: status, Reason: reason, Message: message, ObservedGeneration: mongo.Generation,
	})
}

func (r *MongoReconciler) failReady(ctx context.Context, mongo *orkanov1alpha1.Mongo, before *orkanov1alpha1.MongoStatus, reason string, err error) error {
	if apierrors.IsConflict(err) {
		return err
	}
	setMongoReady(mongo, metav1.ConditionFalse, reason, err.Error())
	if statusErr := r.updateStatus(ctx, mongo, before); statusErr != nil {
		logf.FromContext(ctx).Error(statusErr, "failed to record failure condition", "reason", reason)
	}
	return err
}

func (r *MongoReconciler) updateStatus(ctx context.Context, mongo *orkanov1alpha1.Mongo, before *orkanov1alpha1.MongoStatus) error {
	mongo.Status.ObservedGeneration = mongo.Generation
	if equality.Semantic.DeepEqual(before, &mongo.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, mongo); err != nil {
		return fmt.Errorf("updating Mongo status: %w", err)
	}
	return nil
}

func (r *MongoReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orkanov1alpha1.Mongo{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Named("mongo").
		Complete(r)
}
