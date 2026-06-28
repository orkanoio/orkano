package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/orkanoio/orkano/internal/db"
)

// --- fakeStore methods for the M2.4 read/write views ---

func (f *fakeStore) RecordDeploy(_ context.Context, arg db.RecordDeployParams) (db.DeployHistory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployID++
	row := db.DeployHistory{
		ID:           f.deployID,
		OccurredAt:   pgtype.Timestamptz{Time: fixedNow(), Valid: true},
		AppNamespace: arg.AppNamespace,
		AppName:      arg.AppName,
		BuildName:    arg.BuildName,
		Image:        arg.Image,
		Status:       arg.Status,
	}
	f.deploys = append(f.deploys, row)
	return row, nil
}

func (f *fakeStore) ListAppDeploys(_ context.Context, arg db.ListAppDeploysParams) ([]db.DeployHistory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched []db.DeployHistory
	for i := len(f.deploys) - 1; i >= 0; i-- { // ORDER BY id DESC
		d := f.deploys[i]
		if d.AppNamespace == arg.AppNamespace && d.AppName == arg.AppName {
			matched = append(matched, d)
		}
	}
	return page(matched, arg.Limit, arg.Offset), nil
}

func (f *fakeStore) ListAuditEntries(_ context.Context, arg db.ListAuditEntriesParams) ([]db.AuditLog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []db.AuditLog
	for i := len(f.audit) - 1; i >= 0; i-- { // most-recent-first
		p := f.audit[i]
		rows = append(rows, db.AuditLog{
			ID:         int64(i + 1),
			OccurredAt: pgtype.Timestamptz{Time: fixedNow(), Valid: true},
			Actor:      p.Actor,
			Action:     p.Action,
			Target:     p.Target,
			Outcome:    p.Outcome,
			Detail:     p.Detail,
		})
	}
	return page(rows, arg.Limit, arg.Offset), nil
}

// page applies LIMIT/OFFSET the way the SQL queries do.
func page[T any](rows []T, limit, offset int32) []T {
	if offset >= int32(len(rows)) {
		return nil
	}
	rows = rows[offset:]
	if limit > 0 && int32(len(rows)) > limit {
		rows = rows[:limit]
	}
	return rows
}

// --- tests ---

func TestRecordDeployOnCreateAndUpdate(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	if rec := apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "demo", Spec: webAppSpec()}, ck); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := apiReq(t, s, http.MethodPut, "/api/apps/demo", appUpdateRequest{Spec: webAppSpec()}, ck); rec.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", rec.Code, rec.Body.String())
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.deploys) != 2 {
		t.Fatalf("deploys = %d, want 2 (create + update)", len(store.deploys))
	}
	if store.deploys[0].Status != deployStatusCreated || store.deploys[1].Status != deployStatusUpdated {
		t.Fatalf("statuses = %q, %q, want created, updated", store.deploys[0].Status, store.deploys[1].Status)
	}
	if store.deploys[0].AppNamespace != appsNamespace {
		t.Fatalf("deploy namespace = %q, want %q", store.deploys[0].AppNamespace, appsNamespace)
	}
}

func TestListDeploysPerAppDescending(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "demo", Spec: webAppSpec()}, ck)
	apiReq(t, s, http.MethodPut, "/api/apps/demo", appUpdateRequest{Spec: webAppSpec()}, ck)
	apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "other", Spec: webAppSpec()}, ck)

	rec := apiReq(t, s, http.MethodGet, "/api/apps/demo/deploys", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("list deploys = %d (%s)", rec.Code, rec.Body.String())
	}
	items, _ := decodeBody(t, rec)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("demo deploys = %d, want 2 (other app's deploy excluded)", len(items))
	}
	first, _ := items[0].(map[string]any)
	if first["status"] != deployStatusUpdated {
		t.Fatalf("first deploy = %v, want updated (most-recent-first)", first["status"])
	}
}

// TestListDeploysEmpty proves an app with no recorded deploys returns items:[]
// (not null, not 404) — the empty-page path through parsePage + the store.
func TestListDeploysEmpty(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	rec := apiReq(t, s, http.MethodGet, "/api/apps/demo/deploys", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty deploys = %d, want 200", rec.Code)
	}
	items, ok := decodeBody(t, rec)["items"].([]any)
	if !ok {
		t.Fatal("items missing or null, want []")
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
}

func TestListAuditDescending(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "demo", Spec: webAppSpec()}, ck)

	rec := apiReq(t, s, http.MethodGet, "/api/audit", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("list audit = %d", rec.Code)
	}
	items, _ := decodeBody(t, rec)["items"].([]any)
	if len(items) == 0 {
		t.Fatal("audit list empty after an audited action")
	}
	first, _ := items[0].(map[string]any)
	if first["action"] != "app.create" {
		t.Fatalf("first audit action = %v, want app.create (most-recent-first)", first["action"])
	}
	// INV-03: the detail surfaced to the UI carries the client IP, never a value.
	detail, _ := first["detail"].(map[string]any)
	if detail["ip"] == nil {
		t.Fatalf("audit detail missing ip: %v", first["detail"])
	}
}

func TestListAuditPaginationLimit(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	for _, n := range []string{"a", "b", "c"} {
		apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: n, Spec: webAppSpec()}, ck)
	}
	rec := apiReq(t, s, http.MethodGet, "/api/audit?limit=1", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("paged audit = %d", rec.Code)
	}
	items, _ := decodeBody(t, rec)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("limit=1 returned %d items, want 1", len(items))
	}
}

// TestListAuditOffsetBeyondEnd proves a cursor past the end returns items:[], not
// null or an error (exercises the page(nil) → handler-[] path).
func TestListAuditOffsetBeyondEnd(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)
	ck := authedSession(t, store)

	apiReq(t, s, http.MethodPost, "/api/apps", appCreateRequest{Name: "demo", Spec: webAppSpec()}, ck)

	rec := apiReq(t, s, http.MethodGet, "/api/audit?offset=999", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("offset beyond end = %d, want 200", rec.Code)
	}
	items, ok := decodeBody(t, rec)["items"].([]any)
	if !ok {
		t.Fatal("items missing or null, want []")
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0 (offset past end)", len(items))
	}
}

func TestReadViewsRequireSession(t *testing.T) {
	store := newFakeStore()
	s := apiServer(t, store)

	if rec := apiReq(t, s, http.MethodGet, "/api/audit", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("audit no-session = %d, want 401", rec.Code)
	}
	if rec := apiReq(t, s, http.MethodGet, "/api/apps/demo/deploys", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("deploys no-session = %d, want 401", rec.Code)
	}
}
