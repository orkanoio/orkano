package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/orkanoio/orkano/internal/db"
	"github.com/orkanoio/orkano/internal/repoallowlist"
)

func TestGetRepoAllowlistReadsLiveCanonicalPolicy(t *testing.T) {
	configMap := repoAllowlistFixture(t)
	configMap.Data[repoallowlist.DataKey] = " OrkanoIO/Orkano \nacme/Widgets\norkanoio/orkano\n"
	store := newFakeStore()
	s := apiServer(t, store, configMap)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/repo-allowlist", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("get repository allowlist = %d (%s)", rec.Code, rec.Body.String())
	}
	got := decodeBody(t, rec)["repositories"].([]any)
	if len(got) != 2 || got[0] != "acme/widgets" || got[1] != "orkanoio/orkano" {
		t.Fatalf("repositories = %v, want canonical sorted set", got)
	}
	if got := decodeBody(t, rec)["resourceVersion"]; got != "1" {
		t.Fatalf("resourceVersion = %v, want 1", got)
	}

	// A live ConfigMap change is visible on the next request: the dashboard
	// must never keep serving the process-start environment mirror.
	var live corev1.ConfigMap
	key := client.ObjectKey{Namespace: repoallowlist.Namespace, Name: repoallowlist.ConfigMapName}
	if err := s.cfg.K8s.Get(context.Background(), key, &live); err != nil {
		t.Fatalf("get live ConfigMap: %v", err)
	}
	live.Data[repoallowlist.DataKey] = "new/repository\n"
	if err := s.cfg.K8s.Update(context.Background(), &live); err != nil {
		t.Fatalf("update live ConfigMap: %v", err)
	}
	rec = apiReq(t, s, http.MethodGet, "/api/repo-allowlist", nil, ck)
	got = decodeBody(t, rec)["repositories"].([]any)
	if len(got) != 1 || got[0] != "new/repository" {
		t.Fatalf("repositories after live update = %v, want new policy", got)
	}
}

func TestRepoAllowlistRoutesRequireAuthenticationAndStepUp(t *testing.T) {
	configMap := repoAllowlistFixture(t, "acme/widgets")
	store := newFakeStore()
	s := apiServer(t, store, configMap)

	if rec := apiReq(t, s, http.MethodGet, "/api/repo-allowlist", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous get = %d, want 401", rec.Code)
	}
	ck := authedSession(t, store)
	rec := apiReq(t, s, http.MethodPut, "/api/repo-allowlist",
		updateRepoAllowlistRequest{Repositories: []string{"new/repository"}}, ck)
	if rec.Code != http.StatusForbidden || decodeBody(t, rec)["error"] != "step_up_required" {
		t.Fatalf("update without step-up = %d (%s), want 403 step_up_required", rec.Code, rec.Body.String())
	}

	var unchanged corev1.ConfigMap
	if err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{
		Namespace: repoallowlist.Namespace,
		Name:      repoallowlist.ConfigMapName,
	}, &unchanged); err != nil {
		t.Fatalf("get unchanged ConfigMap: %v", err)
	}
	if unchanged.Data[repoallowlist.DataKey] != "acme/widgets\n" {
		t.Fatalf("policy changed without step-up: %q", unchanged.Data[repoallowlist.DataKey])
	}
}

func TestUpdateRepoAllowlistCanonicalizesAndPreservesConfigMap(t *testing.T) {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       repoallowlist.Namespace,
			Name:            repoallowlist.ConfigMapName,
			ResourceVersion: "7",
			Labels:          map[string]string{"managed-by": "installer"},
			Annotations:     map[string]string{"keep": "yes"},
		},
		Data:       map[string]string{repoallowlist.DataKey: "old/repository\n", "foreign": "preserve"},
		BinaryData: map[string][]byte{"opaque": []byte("preserve")},
	}
	store := newFakeStore()
	s := apiServer(t, store, configMap)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/repo-allowlist", updateRepoAllowlistRequest{
		Repositories:    []string{" OrkanoIO/Orkano ", "acme/Widgets", "", "ACME/widgets"},
		ResourceVersion: "7",
	}, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("update repository allowlist = %d (%s)", rec.Code, rec.Body.String())
	}
	got := decodeBody(t, rec)["repositories"].([]any)
	if len(got) != 2 || got[0] != "acme/widgets" || got[1] != "orkanoio/orkano" {
		t.Fatalf("response repositories = %v, want canonical set", got)
	}
	if got := decodeBody(t, rec)["resourceVersion"]; got != "8" {
		t.Fatalf("response resourceVersion = %v, want 8", got)
	}

	var updated corev1.ConfigMap
	if err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{
		Namespace: repoallowlist.Namespace,
		Name:      repoallowlist.ConfigMapName,
	}, &updated); err != nil {
		t.Fatalf("get updated ConfigMap: %v", err)
	}
	if got := updated.Data[repoallowlist.DataKey]; got != "acme/widgets\norkanoio/orkano\n" {
		t.Fatalf("stored policy = %q, want canonical newline format", got)
	}
	if updated.Data["foreign"] != "preserve" || string(updated.BinaryData["opaque"]) != "preserve" ||
		updated.Labels["managed-by"] != "installer" || updated.Annotations["keep"] != "yes" {
		t.Fatalf("update clobbered ConfigMap metadata or foreign data: %+v", updated)
	}
	entry := lastAudit(t, store, "github.repo_allowlist_update")
	if entry.Target != repoallowlist.ConfigMapName || entry.Outcome != "success" {
		t.Fatalf("success audit drifted: %+v", entry)
	}
	audits := repoAllowlistAudits(store)
	if len(audits) != 2 || audits[0].Outcome != "attempt" || audits[1].Outcome != "success" {
		t.Fatalf("audits = %+v, want attempt then success", audits)
	}
	var attemptDetail, successDetail map[string]any
	if err := json.Unmarshal(audits[0].Detail, &attemptDetail); err != nil {
		t.Fatalf("decode attempt audit detail: %v", err)
	}
	if err := json.Unmarshal(audits[1].Detail, &successDetail); err != nil {
		t.Fatalf("decode success audit detail: %v", err)
	}
	if attemptDetail["requestId"] == "" ||
		attemptDetail["requestId"] != successDetail["requestId"] ||
		attemptDetail["policySha256"] != successDetail["policySha256"] ||
		attemptDetail["policySha256"] != repoAllowlistPolicyHash("acme/widgets\norkanoio/orkano\n") {
		t.Fatalf("audit correlation drifted: attempt=%v success=%v", attemptDetail, successDetail)
	}
}

func TestUpdateRepoAllowlistRejectsStaleReplacement(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, repoAllowlistFixture(t, "acme/revoked"))
	ck := steppedUpSession(t, store)

	firstRead := apiReq(t, s, http.MethodGet, "/api/repo-allowlist", nil, ck)
	secondRead := apiReq(t, s, http.MethodGet, "/api/repo-allowlist", nil, ck)
	firstVersion := decodeBody(t, firstRead)["resourceVersion"].(string)
	secondVersion := decodeBody(t, secondRead)["resourceVersion"].(string)

	remove := apiReq(t, s, http.MethodPut, "/api/repo-allowlist", updateRepoAllowlistRequest{
		Repositories:    []string{},
		ResourceVersion: firstVersion,
	}, ck)
	if remove.Code != http.StatusOK {
		t.Fatalf("remove repository = %d (%s)", remove.Code, remove.Body.String())
	}
	stale := apiReq(t, s, http.MethodPut, "/api/repo-allowlist", updateRepoAllowlistRequest{
		Repositories:    []string{"acme/revoked", "acme/new"},
		ResourceVersion: secondVersion,
	}, ck)
	if stale.Code != http.StatusConflict || decodeBody(t, stale)["error"] != "conflict" {
		t.Fatalf("stale replacement = %d (%s), want 409 conflict", stale.Code, stale.Body.String())
	}

	var current corev1.ConfigMap
	if err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{
		Namespace: repoallowlist.Namespace,
		Name:      repoallowlist.ConfigMapName,
	}, &current); err != nil {
		t.Fatalf("get current ConfigMap: %v", err)
	}
	if current.Data[repoallowlist.DataKey] != "" {
		t.Fatalf("stale replacement reauthorized repository: %q", current.Data[repoallowlist.DataKey])
	}
	assertAudited(t, store, "github.repo_allowlist_update", "failure")
}

func TestUpdateRepoAllowlistRejectsInvalidPolicyAndAudits(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/repo-allowlist", map[string]any{}, ck)
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "invalid_request" {
		t.Fatalf("missing repositories = %d (%s), want 400 invalid_request", rec.Code, rec.Body.String())
	}
	assertAudited(t, store, "github.repo_allowlist_update", "failure")

	rec = apiReq(t, s, http.MethodPut, "/api/repo-allowlist", updateRepoAllowlistRequest{
		Repositories: []string{"acme/widgets"},
	}, ck)
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "invalid_request" {
		t.Fatalf("missing resourceVersion = %d (%s), want 400 invalid_request", rec.Code, rec.Body.String())
	}
	assertAudited(t, store, "github.repo_allowlist_update", "failure")

	rec = apiReq(t, s, http.MethodPut, "/api/repo-allowlist", updateRepoAllowlistRequest{
		Repositories:    []string{"owner-only"},
		ResourceVersion: "1",
	}, ck)
	if rec.Code != http.StatusUnprocessableEntity || decodeBody(t, rec)["error"] != "invalid_repo_allowlist" {
		t.Fatalf("invalid policy = %d (%s), want 422 invalid_repo_allowlist", rec.Code, rec.Body.String())
	}
	assertAudited(t, store, "github.repo_allowlist_update", "failure")
}

func TestUpdateRepoAllowlistDecodeFailuresAreAudited(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "malformed JSON",
			body: `{"repositories":["acme/widgets"]`,
		},
		{
			name: "unknown field",
			body: `{"repositories":["acme/widgets"],"unexpected":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			s := apiServer(t, store)
			ck := steppedUpSession(t, store)
			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodPut,
				"/api/repo-allowlist",
				strings.NewReader(tc.body),
			)
			req.RemoteAddr = "10.0.0.1:5555"
			req.AddCookie(ck)
			rec := httptest.NewRecorder()

			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "invalid_request" {
				t.Fatalf("decode failure = %d (%s), want 400 invalid_request", rec.Code, rec.Body.String())
			}
			entry := lastAudit(t, store, "github.repo_allowlist_update")
			if entry.Target != repoallowlist.ConfigMapName || entry.Outcome != "failure" {
				t.Fatalf("failure audit drifted: %+v", entry)
			}
		})
	}
}

func TestUpdateRepoAllowlistK8sFailureIsAudited(t *testing.T) {
	configMap := repoAllowlistFixture(t, "old/repository")
	failing := interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok {
				return errors.New("write failed")
			}
			return c.Update(ctx, obj, opts...)
		},
	}
	k8s := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(configMap).
		WithInterceptorFuncs(failing).
		Build()
	store := newFakeStore()
	s := serverWith(t, store, k8s)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/repo-allowlist",
		updateRepoAllowlistRequest{
			Repositories:    []string{"new/repository"},
			ResourceVersion: "1",
		}, ck)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed update = %d (%s), want 500", rec.Code, rec.Body.String())
	}
	assertAudited(t, store, "github.repo_allowlist_update", "failure")
}

func TestUpdateRepoAllowlistRefusesMutationWithoutAuditIntent(t *testing.T) {
	configMap := repoAllowlistFixture(t, "old/repository")
	updates := 0
	k8s := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(configMap).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updates++
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()
	store := newFakeStore()
	s := serverWith(t, store, k8s)
	ck := steppedUpSession(t, store)
	store.auditErrors = []error{errors.New("audit unavailable")}

	rec := apiReq(t, s, http.MethodPut, "/api/repo-allowlist", updateRepoAllowlistRequest{
		Repositories:    []string{"new/repository"},
		ResourceVersion: "1",
	}, ck)
	if rec.Code != http.StatusServiceUnavailable || decodeBody(t, rec)["error"] != "unavailable" {
		t.Fatalf("update without audit = %d (%s), want 503 unavailable", rec.Code, rec.Body.String())
	}
	if updates != 0 {
		t.Fatalf("Kubernetes updates = %d, want 0", updates)
	}
	var current corev1.ConfigMap
	if err := k8s.Get(context.Background(), client.ObjectKey{
		Namespace: repoallowlist.Namespace,
		Name:      repoallowlist.ConfigMapName,
	}, &current); err != nil {
		t.Fatalf("get current ConfigMap: %v", err)
	}
	if current.Data[repoallowlist.DataKey] != "old/repository\n" {
		t.Fatalf("policy changed without audit intent: %q", current.Data[repoallowlist.DataKey])
	}
}

func TestUpdateRepoAllowlistKeepsIntentWhenSuccessAuditFails(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store, repoAllowlistFixture(t, "old/repository"))
	ck := steppedUpSession(t, store)
	store.auditErrors = []error{nil, errors.New("audit unavailable")}

	rec := apiReq(t, s, http.MethodPut, "/api/repo-allowlist", updateRepoAllowlistRequest{
		Repositories:    []string{"new/repository"},
		ResourceVersion: "1",
	}, ck)
	if rec.Code != http.StatusServiceUnavailable || decodeBody(t, rec)["error"] != "unavailable" {
		t.Fatalf("update with failed success audit = %d (%s), want 503 unavailable", rec.Code, rec.Body.String())
	}
	var current corev1.ConfigMap
	if err := s.cfg.K8s.Get(context.Background(), client.ObjectKey{
		Namespace: repoallowlist.Namespace,
		Name:      repoallowlist.ConfigMapName,
	}, &current); err != nil {
		t.Fatalf("get current ConfigMap: %v", err)
	}
	if current.Data[repoallowlist.DataKey] != "new/repository\n" {
		t.Fatalf("committed policy was rolled back: %q", current.Data[repoallowlist.DataKey])
	}
	audits := repoAllowlistAudits(store)
	if len(audits) != 1 || audits[0].Outcome != "attempt" {
		t.Fatalf("audits = %+v, want preserved attempt", audits)
	}
}

func TestUpdateRepoAllowlistResolvesCanceledCommittedUpdate(t *testing.T) {
	configMap := repoAllowlistFixture(t, "old/repository")
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	k8s := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(configMap).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				err := c.Update(ctx, obj, opts...)
				if err != nil {
					return err
				}
				cancelRequest()
				return context.Canceled
			},
		}).
		Build()
	store := newFakeStore()
	s := serverWith(t, store, k8s)
	ck := steppedUpSession(t, store)
	store.auditContextErrors = nil
	body, err := json.Marshal(updateRepoAllowlistRequest{
		Repositories:    []string{"new/repository"},
		ResourceVersion: "1",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequestWithContext(
		requestCtx,
		http.MethodPut,
		"/api/repo-allowlist",
		bytes.NewReader(body),
	)
	req.RemoteAddr = "10.0.0.1:5555"
	req.AddCookie(ck)
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("canceled committed update = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	store.mu.Lock()
	contextErrors := append([]error{}, store.auditContextErrors...)
	store.mu.Unlock()
	if len(contextErrors) != 2 || contextErrors[0] != nil || contextErrors[1] != nil {
		t.Fatalf("audit context errors = %v, want two detached live contexts", contextErrors)
	}
	audits := repoAllowlistAudits(store)
	if len(audits) != 2 || audits[1].Outcome != "success" {
		t.Fatalf("audits = %+v, want detached success", audits)
	}
}

func TestUpdateRepoAllowlistAuditsIndeterminateWhenReadbackFails(t *testing.T) {
	gets := 0
	k8s := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(repoAllowlistFixture(t, "old/repository")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				gets++
				if gets > 1 {
					return errors.New("read-back unavailable")
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return context.DeadlineExceeded
			},
		}).
		Build()
	store := newFakeStore()
	s := serverWith(t, store, k8s)
	ck := steppedUpSession(t, store)

	rec := apiReq(t, s, http.MethodPut, "/api/repo-allowlist", updateRepoAllowlistRequest{
		Repositories:    []string{"new/repository"},
		ResourceVersion: "1",
	}, ck)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("indeterminate update = %d (%s), want 500", rec.Code, rec.Body.String())
	}
	audits := repoAllowlistAudits(store)
	if len(audits) != 2 || audits[0].Outcome != "attempt" || audits[1].Outcome != "indeterminate" {
		t.Fatalf("audits = %+v, want attempt then indeterminate", audits)
	}
}

func TestGetRepoAllowlistFailsClosedOnMissingOrInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		k8s  client.Client
		code string
	}{
		{
			name: "missing",
			k8s:  fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
			code: "cluster_not_ready",
		},
		{
			name: "invalid",
			k8s: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: repoallowlist.Namespace,
					Name:      repoallowlist.ConfigMapName,
				},
				Data: map[string]string{repoallowlist.DataKey: "not-a-repository\n"},
			}).Build(),
			code: "unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			s := serverWith(t, store, tc.k8s)
			ck := authedSession(t, store)
			rec := apiReq(t, s, http.MethodGet, "/api/repo-allowlist", nil, ck)
			if rec.Code != http.StatusServiceUnavailable || decodeBody(t, rec)["error"] != tc.code {
				t.Fatalf("get = %d (%s), want 503 %s", rec.Code, rec.Body.String(), tc.code)
			}
		})
	}
}

func repoAllowlistAudits(store *fakeStore) []db.AppendAuditEntryParams {
	store.mu.Lock()
	defer store.mu.Unlock()
	var entries []db.AppendAuditEntryParams
	for _, entry := range store.audit {
		if entry.Action == "github.repo_allowlist_update" {
			entries = append(entries, entry)
		}
	}
	return entries
}
