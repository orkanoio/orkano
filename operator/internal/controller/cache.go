package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// The three namespaces Orkano operates in. The operator holds Roles only here
// (config/rbac/operator.yaml) and never cluster-admin, so the manager cache
// must be scoped to them — a cluster-wide informer (controller-runtime's
// default) would list/watch namespaces the operator cannot read and fail with
// Forbidden the moment it runs under the namespaced grants (M1.5).
const (
	systemNamespace = "orkano-system"
	appsNamespace   = "orkano-apps"
	buildNamespace  = "orkano-builds"
)

// cacheScope pairs a watched type with the namespaces the cache watches it in.
// group/resource name the RBAC rule that authorizes the watch: the namespace
// set MUST be a subset of the namespaces config/rbac/operator.yaml grants the
// operator list+watch on that resource, or the informer Forbids at startup.
// TestCacheScopeWithinOperatorRBAC enforces that subset relationship against
// the RBAC matrix, so adding a watched type here forces declaring its grant.
type cacheScope struct {
	obj        client.Object
	group      string
	resource   string
	namespaces []string
}

// cacheScopes is the single source of truth for what the manager cache watches
// and where, mirroring every For/Owns/Watches across the controllers:
//   - App/Build/Domain/Postgres/Mongo and their owned Service/Ingress/StatefulSet
//     live in orkano-apps.
//   - Build Jobs live in orkano-builds.
//   - Deployments span app workloads (orkano-apps) and the registry the
//     RegistryCert controller rolls (orkano-system).
//   - Certificates span Domain TLS (orkano-apps) and registry TLS
//     (orkano-system); watched as unstructured (no cert-manager Go dep).
//
// The database connection Secrets and data PVCs are owned (GC cascades) but
// NOT watched — the operator holds no secrets/PVC list+watch, so they are read
// uncached via APIReader and never appear here.
func cacheScopes() []cacheScope {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)

	apps := []string{appsNamespace}
	appsAndSystem := []string{appsNamespace, systemNamespace}

	return []cacheScope{
		{&orkanov1alpha1.App{}, "orkano.io", "apps", apps},
		{&orkanov1alpha1.Build{}, "orkano.io", "builds", apps},
		{&orkanov1alpha1.Domain{}, "orkano.io", "domains", apps},
		{&orkanov1alpha1.Postgres{}, "orkano.io", "postgreses", apps},
		{&orkanov1alpha1.Mongo{}, "orkano.io", "mongoes", apps},
		{&corev1.Service{}, "", "services", apps},
		{&networkingv1.Ingress{}, "networking.k8s.io", "ingresses", apps},
		{&batchv1.Job{}, "batch", "jobs", []string{buildNamespace}},
		{&appsv1.Deployment{}, "apps", "deployments", appsAndSystem},
		{&appsv1.StatefulSet{}, "apps", "statefulsets", apps},
		{cert, "cert-manager.io", "certificates", appsAndSystem},
	}
}

// CacheOptions scopes the manager cache per type to exactly the namespaces the
// operator's RBAC allows. main.go and the envtest harness share it so the test
// suite exercises the real production scoping. A cluster-wide List of a type
// (e.g. the dispatcher's List(App)) still returns that type's scoped set —
// orkano-apps — without per-call InNamespace.
func CacheOptions() cache.Options {
	byObject := make(map[client.Object]cache.ByObject, len(cacheScopes()))
	for _, s := range cacheScopes() {
		namespaces := make(map[string]cache.Config, len(s.namespaces))
		for _, ns := range s.namespaces {
			namespaces[ns] = cache.Config{}
		}
		byObject[s.obj] = cache.ByObject{Namespaces: namespaces}
	}
	// ByObject is the EXHAUSTIVE watch set: every type any controller watches
	// (For/Owns/Watches) must appear in cacheScopes, or its informer falls
	// through to a cluster-wide watch and Forbids under the namespaced grants.
	// Deliberately no DefaultNamespaces fallback — a three-namespace default
	// would not prevent that Forbidden (the operator has no list/watch in
	// orkano-system for pods, Orkano CRDs, ingresses, or jobs), it would only
	// relocate it, while hiding the real contract: list the type here.
	return cache.Options{ByObject: byObject}
}
