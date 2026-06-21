// The M1.1 milestone gate: each of the App contract files (01–05) must
// reconcile to its complete object graph — the objects, their shapes, their
// ownership, and the status a user would read from kubectl get. Images arrive
// the way M1.2 will deliver them, through a succeeded Build, never by poking
// status directly. The sixth file (06, the Postgres catalog kind) is exercised
// by TestExample06PostgresCatalogObjectGraph, which reconciles it to the full
// StatefulSet + headless Service + connection Secret graph now that the M1.4
// reconciler has landed. envtest runs no GC, so each example is applied exactly
// once per suite run: these tests are the only consumers of the example files.
package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// applyExample creates every document of a docs/examples file, exactly as
// kubectl apply would — the acceptance bar for the archetype tasks is the
// real contract files, not Go specs that approximate them.
func applyExample(t *testing.T, file string) {
	t.Helper()
	ctx := context.Background()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "examples", file))
	if err != nil {
		t.Fatalf("failed to read example %s: %v", file, err)
	}
	dec := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("failed to decode %s: %v", file, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("failed to create %s %s from %s: %v", obj.GetKind(), obj.GetName(), file, err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, obj) })
	}
}

// supplyImage plays the M1.2 pipeline the way the M1.3 dispatcher will:
// snapshot the App's source and strategy into an immutable Build and mark it
// succeeded with a digest-pinned image. Creating the snapshot doubles as
// proof that the Build schema accepts every example's source/strategy
// permutation (Static dir, subPath, non-default dockerfile path).
func supplyImage(t *testing.T, appName string) string {
	t.Helper()
	ctx := context.Background()
	var app orkanov1alpha1.App
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: appName, Namespace: appsNamespace}, &app); err != nil {
		t.Fatalf("failed to get App %s: %v", appName, err)
	}
	build := &orkanov1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: appName + "-gate-001", Namespace: appsNamespace},
		Spec: orkanov1alpha1.BuildSpec{
			AppName:  appName,
			Commit:   "0123456789abcdef0123456789abcdef01234567",
			Source:   app.Spec.Source,
			Strategy: app.Spec.Build,
		},
	}
	if err := k8sClient.Create(ctx, build); err != nil {
		t.Fatalf("failed to create Build %s: %v", build.Name, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, build) })
	setBuildPhase(t, build.Name, orkanov1alpha1.BuildSucceeded, testImage)
	// Barrier: without it callers would synchronize by accident, through
	// whichever eventually-backed helper they happen to call next.
	eventually(t, "App "+appName+" to observe "+build.Name, func(ctx context.Context) (bool, error) {
		var fresh orkanov1alpha1.App
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: appName, Namespace: appsNamespace}, &fresh); err != nil {
			return false, err
		}
		return fresh.Status.Image == testImage && fresh.Status.LatestBuild == build.Name, nil
	})
	return build.Name
}

func assertOwnedBy(t *testing.T, obj client.Object, kind, name string) {
	t.Helper()
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.Kind != kind || owner.Name != name {
		t.Fatalf("%s not controller-owned by %s %s: %+v", obj.GetName(), kind, name, owner)
	}
}

// envEntries returns every env entry with the given name — duplicates are a
// rendering bug, so counts matter as much as values.
func envEntries(c corev1.Container, name string) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, e := range c.Env {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// assertWebService waits for the App's Service and pins the canonical Web
// shape: ClusterIP, 80 routed to the named container port, selector matching
// the pod template so traffic actually lands on the Deployment's pods.
func assertWebService(t *testing.T, appName string, deploy *appsv1.Deployment) {
	t.Helper()
	var svc corev1.Service
	key := types.NamespacedName{Name: appName, Namespace: appsNamespace}
	eventually(t, "Service "+appName, func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, key, &svc)
		return err == nil, client.IgnoreNotFound(err)
	})
	assertOwnedBy(t, &svc, "App", appName)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("service type = %q, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.String() != servicePortName {
		t.Errorf("service ports = %+v, want 80 -> named port %q", svc.Spec.Ports, servicePortName)
	}
	if svc.Spec.Selector[appLabel] != appName {
		t.Errorf("service selector = %+v, want %s=%s", svc.Spec.Selector, appLabel, appName)
	}
	if !equality.Semantic.DeepEqual(svc.Spec.Selector, deploy.Spec.Template.Labels) {
		t.Errorf("service selector %+v does not match pod template labels %+v", svc.Spec.Selector, deploy.Spec.Template.Labels)
	}
}

// assertDomainIngress waits for the Domain's Ingress and pins the canonical
// shape: one host rule routing / to the App's Service named port, TLS bound
// to <domain>-tls, the platform issuer annotation, no ingressClassName.
func assertDomainIngress(t *testing.T, domainName, host, appName string) {
	t.Helper()
	ing := getIngress(t, domainName)
	assertOwnedBy(t, ing, "Domain", domainName)
	if got := ing.Annotations[clusterIssuerAnnotation]; got != testClusterIssuer {
		t.Errorf("issuer annotation = %q, want %q", got, testClusterIssuer)
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != host {
		t.Fatalf("rules = %+v, want one rule for %s", ing.Spec.Rules, host)
	}
	paths := ing.Spec.Rules[0].HTTP.Paths
	if len(paths) != 1 || paths[0].Path != "/" || *paths[0].PathType != networkingv1.PathTypePrefix {
		t.Errorf("paths = %+v, want a single / Prefix path", paths)
	}
	backend := paths[0].Backend.Service
	if backend == nil || backend.Name != appName || backend.Port.Name != servicePortName {
		t.Errorf("backend = %+v, want Service %s port %q", backend, appName, servicePortName)
	}
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != domainName+tlsSecretSuffix ||
		len(ing.Spec.TLS[0].Hosts) != 1 || ing.Spec.TLS[0].Hosts[0] != host {
		t.Errorf("tls = %+v, want host %s with secret %s", ing.Spec.TLS, host, domainName+tlsSecretSuffix)
	}
	if ing.Spec.IngressClassName != nil {
		t.Errorf("ingressClassName = %v, want unset (cluster default)", *ing.Spec.IngressClassName)
	}
}

// driveDomainAvailable plays ingress-shim and cert-manager: create the
// Certificate named after the TLS secret, mark it Ready, and wait for the
// Domain to report Available.
func driveDomainAvailable(t *testing.T, domainName string) {
	t.Helper()
	certName := domainName + tlsSecretSuffix
	createCertificate(t, certName)
	setCertificateReady(t, certName, metav1.ConditionTrue, "Ready", "certificate issued")
	waitForDomainCondition(t, domainName, orkanov1alpha1.ConditionCertificateReady, metav1.ConditionTrue, "Ready")
	waitForDomainCondition(t, domainName, orkanov1alpha1.ConditionReady, metav1.ConditionTrue, reasonAvailable)
}

func TestExample01StaticSiteObjectGraph(t *testing.T) {
	applyExample(t, "01-static-site.yaml")
	build := supplyImage(t, "blog")

	deploy := getDeployment(t, "blog")
	assertOwnedBy(t, deploy, "App", "blog")
	if *deploy.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want default 1", *deploy.Spec.Replicas)
	}
	c := deploy.Spec.Template.Spec.Containers[0]
	if c.Image != testImage {
		t.Errorf("image = %q, want %q", c.Image, testImage)
	}
	if len(c.Command) != 0 {
		t.Errorf("command = %v, want none", c.Command)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != defaultWebPort || c.Ports[0].Name != servicePortName {
		t.Errorf("ports = %+v, want named port %q on default %d", c.Ports, servicePortName, defaultWebPort)
	}
	if port := envEntries(c, portEnvName); len(port) != 1 || port[0].Value != "8080" {
		t.Errorf("PORT env = %+v, want exactly one injected 8080", port)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil ||
		c.ReadinessProbe.TCPSocket.Port.IntValue() != int(defaultWebPort) {
		t.Errorf("readiness probe = %+v, want TCPSocket on %d", c.ReadinessProbe, defaultWebPort)
	}
	if c.LivenessProbe != nil {
		t.Errorf("liveness probe = %+v, want none without healthCheck", c.LivenessProbe)
	}
	if len(c.Resources.Requests) != 0 || len(c.Resources.Limits) != 0 {
		t.Errorf("resources = %+v, want none when spec.resources is unset", c.Resources)
	}

	assertWebService(t, "blog", deploy)
	assertDomainIngress(t, "blog-example-com", "blog.example.com", "blog")
	// Asserted before any Certificate exists: the URL derives from the
	// winning Domain alone — cert issuance runs in parallel, never gates it.
	waitForAppURL(t, "blog", "https://blog.example.com")

	markDeploymentAvailable(t, "blog", 1)
	app := waitForReady(t, "blog", metav1.ConditionTrue, reasonAvailable)
	if app.Status.LatestBuild != build || app.Status.Image != testImage {
		t.Errorf("latestBuild/image = %q/%q, want %q/%q", app.Status.LatestBuild, app.Status.Image, build, testImage)
	}

	driveDomainAvailable(t, "blog-example-com")
	waitForAppURL(t, "blog", "https://blog.example.com")
}

func TestExample02WebServicePostgresObjectGraph(t *testing.T) {
	applyExample(t, "02-web-service-postgres.yaml")
	build := supplyImage(t, "api")

	deploy := getDeployment(t, "api")
	assertOwnedBy(t, deploy, "App", "api")
	if *deploy.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2 from the example", *deploy.Spec.Replicas)
	}
	c := deploy.Spec.Template.Spec.Containers[0]
	if c.Image != testImage {
		t.Errorf("image = %q, want %q", c.Image, testImage)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 3000 || c.Ports[0].Name != servicePortName {
		t.Errorf("ports = %+v, want named port %q on 3000", c.Ports, servicePortName)
	}
	if port := envEntries(c, portEnvName); len(port) != 1 || port[0].Value != "3000" {
		t.Errorf("PORT env = %+v, want exactly one injected 3000 matching spec.port", port)
	}
	if nodeEnv := envEntries(c, "NODE_ENV"); len(nodeEnv) != 1 || nodeEnv[0].Value != "production" {
		t.Errorf("NODE_ENV env = %+v, want production", nodeEnv)
	}
	dbURL := envEntries(c, "DATABASE_URL")
	if len(dbURL) != 1 || dbURL[0].Value != "" || dbURL[0].ValueFrom == nil || dbURL[0].ValueFrom.SecretKeyRef == nil ||
		dbURL[0].ValueFrom.SecretKeyRef.Name != "api-db" || dbURL[0].ValueFrom.SecretKeyRef.Key != "uri" {
		t.Errorf("DATABASE_URL = %+v, want secretKeyRef api-db/uri and no inline value (INV-03)", dbURL)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/healthz" ||
		c.ReadinessProbe.HTTPGet.Port.IntValue() != 3000 {
		t.Errorf("readiness probe = %+v, want HTTP GET /healthz on 3000", c.ReadinessProbe)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil || c.LivenessProbe.HTTPGet.Path != "/healthz" ||
		c.LivenessProbe.HTTPGet.Port.IntValue() != 3000 {
		t.Errorf("liveness probe = %+v, want HTTP GET /healthz on 3000", c.LivenessProbe)
	}
	if got := c.Resources.Requests.Cpu().String(); got != "250m" {
		t.Errorf("cpu request = %s, want 250m", got)
	}
	if got := c.Resources.Requests.Memory().String(); got != "512Mi" {
		t.Errorf("memory request = %s, want 512Mi", got)
	}
	if got := c.Resources.Limits.Memory().String(); got != "512Mi" {
		t.Errorf("memory limit = %s, want 512Mi (= request)", got)
	}
	if _, hasCPULimit := c.Resources.Limits[corev1.ResourceCPU]; hasCPULimit {
		t.Errorf("cpu limit set, want none: %+v", c.Resources.Limits)
	}

	assertWebService(t, "api", deploy)
	assertDomainIngress(t, "api-example-com", "api.example.com", "api")
	waitForAppURL(t, "api", "https://api.example.com")

	markDeploymentAvailable(t, "api", 2)
	app := waitForReady(t, "api", metav1.ConditionTrue, reasonAvailable)
	if app.Status.AvailableReplicas != 2 {
		t.Errorf("availableReplicas = %d, want 2 mirrored from the Deployment", app.Status.AvailableReplicas)
	}
	if app.Status.LatestBuild != build || app.Status.Image != testImage {
		t.Errorf("latestBuild/image = %q/%q, want %q/%q", app.Status.LatestBuild, app.Status.Image, build, testImage)
	}

	driveDomainAvailable(t, "api-example-com")
	waitForAppURL(t, "api", "https://api.example.com")
}

func TestExample03BackgroundWorkerObjectGraph(t *testing.T) {
	applyExample(t, "03-background-worker.yaml")
	build := supplyImage(t, "mailer")

	deploy := getDeployment(t, "mailer")
	assertOwnedBy(t, deploy, "App", "mailer")
	if *deploy.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want default 1", *deploy.Spec.Replicas)
	}
	c := deploy.Spec.Template.Spec.Containers[0]
	if c.Image != testImage {
		t.Errorf("image = %q, want %q", c.Image, testImage)
	}
	if len(c.Command) != 2 || c.Command[0] != "node" || c.Command[1] != "worker.js" {
		t.Errorf("command = %v, want [node worker.js]", c.Command)
	}
	if len(c.Ports) != 0 {
		t.Errorf("ports = %+v, want none on a Worker", c.Ports)
	}
	if c.ReadinessProbe != nil || c.LivenessProbe != nil {
		t.Errorf("probes = %+v / %+v, want none on a Worker", c.ReadinessProbe, c.LivenessProbe)
	}
	if len(c.Env) != 1 {
		t.Fatalf("env = %+v, want exactly DATABASE_URL (no PORT injection on a Worker)", c.Env)
	}
	dbURL := c.Env[0]
	if dbURL.Name != "DATABASE_URL" || dbURL.ValueFrom == nil || dbURL.ValueFrom.SecretKeyRef == nil ||
		dbURL.ValueFrom.SecretKeyRef.Name != "api-db" || dbURL.ValueFrom.SecretKeyRef.Key != "uri" {
		t.Errorf("env[0] = %+v, want DATABASE_URL from secretKeyRef api-db/uri", dbURL)
	}

	markDeploymentAvailable(t, "mailer", 1)
	app := waitForReady(t, "mailer", metav1.ConditionTrue, reasonAvailable)
	if app.Status.LatestBuild != build || app.Status.Image != testImage {
		t.Errorf("latestBuild/image = %q/%q, want %q/%q", app.Status.LatestBuild, app.Status.Image, build, testImage)
	}
	if app.Status.URL != "" {
		t.Errorf("status.url = %q on a Worker, want empty", app.Status.URL)
	}

	// Reconcile walks the Service branch before it writes status, so the
	// Available condition above proves the decision not to create one.
	var svc corev1.Service
	err := k8sClient.Get(context.Background(), types.NamespacedName{Name: "mailer", Namespace: appsNamespace}, &svc)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no Service for the Worker example, got: %v", err)
	}
}

func TestExample04MonorepoSubpathObjectGraph(t *testing.T) {
	applyExample(t, "04-monorepo-subpath.yaml")
	build := supplyImage(t, "billing")

	// subPath and the Dockerfile-relative-to-it resolution shape the build,
	// not the workload: the rendered graph is every operator-side default.
	deploy := getDeployment(t, "billing")
	assertOwnedBy(t, deploy, "App", "billing")
	if *deploy.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want default 1", *deploy.Spec.Replicas)
	}
	c := deploy.Spec.Template.Spec.Containers[0]
	if c.Image != testImage {
		t.Errorf("image = %q, want %q", c.Image, testImage)
	}
	if len(c.Command) != 0 {
		t.Errorf("command = %v, want none", c.Command)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != defaultWebPort || c.Ports[0].Name != servicePortName {
		t.Errorf("ports = %+v, want named port %q on default %d", c.Ports, servicePortName, defaultWebPort)
	}
	if port := envEntries(c, portEnvName); len(port) != 1 || port[0].Value != "8080" {
		t.Errorf("PORT env = %+v, want exactly one injected 8080", port)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil ||
		c.ReadinessProbe.TCPSocket.Port.IntValue() != int(defaultWebPort) {
		t.Errorf("readiness probe = %+v, want TCPSocket on %d", c.ReadinessProbe, defaultWebPort)
	}
	if c.LivenessProbe != nil {
		t.Errorf("liveness probe = %+v, want none without healthCheck", c.LivenessProbe)
	}
	if len(c.Resources.Requests) != 0 || len(c.Resources.Limits) != 0 {
		t.Errorf("resources = %+v, want none when spec.resources is unset", c.Resources)
	}

	assertWebService(t, "billing", deploy)

	markDeploymentAvailable(t, "billing", 1)
	app := waitForReady(t, "billing", metav1.ConditionTrue, reasonAvailable)
	if app.Status.LatestBuild != build || app.Status.Image != testImage {
		t.Errorf("latestBuild/image = %q/%q, want %q/%q", app.Status.LatestBuild, app.Status.Image, build, testImage)
	}
	// No Domain in the example: the URL column stays empty.
	if app.Status.URL != "" {
		t.Errorf("status.url = %q without any Domain, want empty", app.Status.URL)
	}
}

// TestExample06PostgresCatalogObjectGraph is the M1.4 milestone gate for the
// catalog kind: the sixth contract file reconciles to the StatefulSet + headless
// Service + connection Secret named exactly metadata.name that examples 02/03
// already reference, and reports Ready once the database pod is up. The Secret's
// key set and the api-db -> api_db identifier are ADR-0014's frozen contract.
func TestExample06PostgresCatalogObjectGraph(t *testing.T) {
	applyExample(t, "06-postgres-catalog.yaml")

	var pg orkanov1alpha1.Postgres
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "api-db", Namespace: appsNamespace}, &pg); err != nil {
		t.Fatalf("failed to get Postgres api-db: %v", err)
	}
	if pg.Spec.Version != "16" {
		t.Errorf("version = %q, want 16 from the example", pg.Spec.Version)
	}
	if pg.Spec.StorageSize == nil || pg.Spec.StorageSize.String() != "10Gi" {
		t.Errorf("storageSize = %v, want defaulted 10Gi", pg.Spec.StorageSize)
	}

	sts := getStatefulSet(t, "api-db")
	assertOwnedBy(t, sts, "Postgres", "api-db")
	if got := sts.Spec.Template.Spec.Containers[0].Image; got != postgresImages["16"] {
		t.Errorf("image = %q, want the digest-pinned postgres:16", got)
	}

	secret := getPostgresSecret(t, "api-db")
	assertOwnedBy(t, secret, "Postgres", "api-db")
	// The very Secret + key examples 02 and 03 wire DATABASE_URL to.
	if got := string(secret.Data[orkanov1alpha1.SecretKeyURI]); !strings.HasPrefix(got, "postgresql://api_db:") ||
		!strings.Contains(got, "@api-db.orkano-apps.svc.cluster.local:5432/api_db") {
		t.Errorf("secret uri = %q, want postgresql://api_db:...@api-db...:5432/api_db", got)
	}

	got := waitForPostgresCondition(t, "api-db", orkanov1alpha1.ConditionReady, metav1.ConditionFalse, reasonProvisioning)
	if got.Status.SecretName != "api-db" {
		t.Errorf("status.secretName = %q, want api-db", got.Status.SecretName)
	}
	markStatefulSetReady(t, "api-db", 1)
	waitForPostgresCondition(t, "api-db", orkanov1alpha1.ConditionReady, metav1.ConditionTrue, reasonAvailable)
}

func TestExample05DockerfileObjectGraph(t *testing.T) {
	applyExample(t, "05-dockerfile.yaml")
	build := supplyImage(t, "imageproc")

	deploy := getDeployment(t, "imageproc")
	assertOwnedBy(t, deploy, "App", "imageproc")
	if *deploy.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want default 1", *deploy.Spec.Replicas)
	}
	c := deploy.Spec.Template.Spec.Containers[0]
	if c.Image != testImage {
		t.Errorf("image = %q, want %q", c.Image, testImage)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 8080 || c.Ports[0].Name != servicePortName {
		t.Errorf("ports = %+v, want named port %q on explicit 8080", c.Ports, servicePortName)
	}
	if port := envEntries(c, portEnvName); len(port) != 1 || port[0].Value != "8080" {
		t.Errorf("PORT env = %+v, want exactly one injected 8080 matching spec.port", port)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil ||
		c.ReadinessProbe.TCPSocket.Port.IntValue() != 8080 {
		t.Errorf("readiness probe = %+v, want TCPSocket on 8080", c.ReadinessProbe)
	}
	if c.LivenessProbe != nil {
		t.Errorf("liveness probe = %+v, want none without healthCheck", c.LivenessProbe)
	}

	assertWebService(t, "imageproc", deploy)
	assertDomainIngress(t, "img-example-com", "img.example.com", "imageproc")
	waitForAppURL(t, "imageproc", "https://img.example.com")

	markDeploymentAvailable(t, "imageproc", 1)
	app := waitForReady(t, "imageproc", metav1.ConditionTrue, reasonAvailable)
	if app.Status.LatestBuild != build || app.Status.Image != testImage {
		t.Errorf("latestBuild/image = %q/%q, want %q/%q", app.Status.LatestBuild, app.Status.Image, build, testImage)
	}

	driveDomainAvailable(t, "img-example-com")
	waitForAppURL(t, "imageproc", "https://img.example.com")
}
