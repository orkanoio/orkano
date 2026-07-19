package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orkanov1alpha1 "github.com/orkanoio/orkano/api/v1alpha1"
)

// cacheUnknownNamespace is the multi-namespace cache's error substring when a
// read targets a namespace it does not watch for that type. Asserting on it
// proves the cache is genuinely scoped, not just returning empty results.
const cacheUnknownNamespace = "unknown namespace for the cache"

// TestCacheScopedPerType proves the cache is scoped per type, not by a blanket
// namespace set: Apps are served in orkano-apps but refused in orkano-builds,
// and Jobs are the mirror image. A naive DefaultNamespaces over all three
// namespaces would serve both everywhere and pass a weaker test while still
// Forbidding in production (the operator has no apps grant in orkano-builds,
// nor a jobs grant in orkano-apps).
//
// The positive legs create a real object and assert the cache returns it, not
// merely that List does not error — a wrong-but-superset namespace set would
// also not error.
func TestCacheScopedPerType(t *testing.T) {
	ctx := context.Background()
	const probe = "cache-scope-probe"

	app := &orkanov1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: probe, Namespace: appsNamespace},
		Spec: orkanov1alpha1.AppSpec{
			Type:   orkanov1alpha1.WorkloadWorker,
			Source: orkanov1alpha1.Source{GitHub: &orkanov1alpha1.GitHubSource{Repo: "orkanoio/example"}},
			Build:  orkanov1alpha1.BuildStrategy{Strategy: orkanov1alpha1.StrategyDockerfile},
		},
	}
	if err := k8sClient.Create(ctx, app); err != nil {
		t.Fatalf("create App in %s: %v", appsNamespace, err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), app) })

	// A Job with no Orkano annotations maps to no Build (mapJobToBuild), so
	// this triggers no reconcile — it only seeds the orkano-builds Job cache.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: probe, Namespace: buildNamespace},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "noop", Image: "noop"}},
		}}},
	}
	if err := k8sClient.Create(ctx, job); err != nil {
		t.Fatalf("create Job in %s: %v", buildNamespace, err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	})

	// Positive: each type is served by the cache from the namespace it is
	// scoped to (eventually, once the informer observes the create).
	eventually(t, "App served from the orkano-apps cache", func(ctx context.Context) (bool, error) {
		var apps orkanov1alpha1.AppList
		if err := cachedClient.List(ctx, &apps, client.InNamespace(appsNamespace)); err != nil {
			return false, err
		}
		return containsObject(len(apps.Items), func(i int) string { return apps.Items[i].Name }, probe), nil
	})
	eventually(t, "Job served from the orkano-builds cache", func(ctx context.Context) (bool, error) {
		var jobs batchv1.JobList
		if err := cachedClient.List(ctx, &jobs, client.InNamespace(buildNamespace)); err != nil {
			return false, err
		}
		return containsObject(len(jobs.Items), func(i int) string { return jobs.Items[i].Name }, probe), nil
	})

	// Negative: the same types in the namespace they are NOT scoped to are
	// refused by the cache outright — proving per-type scoping, not a blanket
	// three-namespace cache (these fire from a structural cache property,
	// independent of whether any object exists there).
	err := cachedClient.List(ctx, &orkanov1alpha1.AppList{}, client.InNamespace(buildNamespace))
	if err == nil || !strings.Contains(err.Error(), cacheUnknownNamespace) {
		t.Fatalf("cached List of Apps in %s should be refused by the scoped cache, got: %v", buildNamespace, err)
	}
	err = cachedClient.List(ctx, &batchv1.JobList{}, client.InNamespace(appsNamespace))
	if err == nil || !strings.Contains(err.Error(), cacheUnknownNamespace) {
		t.Fatalf("cached List of Jobs in %s should be refused by the scoped cache, got: %v", appsNamespace, err)
	}
}

// containsObject reports whether name(i) for i in [0,n) equals want.
func containsObject(n int, name func(int) string, want string) bool {
	for i := 0; i < n; i++ {
		if name(i) == want {
			return true
		}
	}
	return false
}

// TestClusterWideListReturnsScopedSet pins the contract the dispatcher relies
// on: a cluster-wide List (no InNamespace) of a single-namespace-scoped type
// returns that type's scoped set without erroring, so appsForRepo/budget reuse
// the App/Build informers and get the orkano-apps objects with no per-call
// InNamespace and no extra informer.
func TestClusterWideListReturnsScopedSet(t *testing.T) {
	ctx := context.Background()
	for _, list := range []client.ObjectList{&orkanov1alpha1.AppList{}, &orkanov1alpha1.BuildList{}} {
		if err := cachedClient.List(ctx, list); err != nil {
			t.Fatalf("cluster-wide cached List of %T should succeed against the scoped cache: %v", list, err)
		}
	}
}

// TestCacheScopeWithinOperatorRBAC is the safety property that makes the
// scoping correct rather than merely active: every (resource, namespace) the
// cache watches must be a (resource, namespace) the operator's RBAC grants
// list AND watch on. rbac_matrix_test.go separately proves the matrix doc, the
// config/rbac manifests, and the live authorizer all agree, so checking the
// cache scope against the parsed matrix transitively guarantees no informer
// will Forbid under the namespaced grants in production (M1.5).
func TestCacheScopeWithinOperatorRBAC(t *testing.T) {
	docTuples := parseRBACMatrixDoc(t)
	for _, s := range cacheScopes() {
		for _, ns := range s.namespaces {
			for _, verb := range []string{"list", "watch"} {
				tu := rbacTuple{
					identity:  operatorIdentity,
					namespace: ns,
					group:     s.group,
					resource:  s.resource,
					verb:      verb,
				}
				if !docTuples[tu] {
					t.Errorf("cache watches %s in %s, but the RBAC matrix grants the operator no %q there (%s)",
						s.resource, ns, verb, tu)
				}
			}
		}
	}
}
