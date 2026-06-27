package db_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orkanoio/orkano/internal/db"
)

func tsAt(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// TestDashboardSchemaRoundTrip exercises every generated dashboard query through
// the real schema (migration 00003): the account store, opaque sessions, the
// append-only audit log, and the deploy timeline.
func TestDashboardSchemaRoundTrip(t *testing.T) {
	dsn := startPostgres(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := db.New(pool)

	t.Run("users", func(t *testing.T) {
		if n, err := q.CountUsers(ctx); err != nil || n != 0 {
			t.Fatalf("CountUsers on empty: got %d, %v", n, err)
		}
		u, err := q.CreateUser(ctx, db.CreateUserParams{
			Username:        "Admin",
			PasswordHash:    "$2y$10$bcrypthashplaceholderxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
			TotpSecret:      "JBSWY3DPEHPK3PXP",
			TotpConfirmedAt: tsAt(time.Now().UTC()),
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if u.ID == 0 || !u.TotpConfirmedAt.Valid {
			t.Fatalf("CreateUser returned unexpected row: %+v", u)
		}

		// Lookup is case-insensitive (unique index on lower(username)).
		got, err := q.GetUserByUsername(ctx, "admin")
		if err != nil || got.ID != u.ID {
			t.Fatalf("GetUserByUsername mixed-case: got %+v, %v", got, err)
		}
		byID, err := q.GetUserByID(ctx, u.ID)
		if err != nil || byID.Username != "Admin" {
			t.Fatalf("GetUserByID: got %+v, %v", byID, err)
		}
		if n, err := q.CountUsers(ctx); err != nil || n != 1 {
			t.Fatalf("CountUsers after create: got %d, %v", n, err)
		}

		// A second account whose username only differs by case collides on the
		// lowercased unique index (single local admin, ADR-0003).
		_, err = q.CreateUser(ctx, db.CreateUserParams{
			Username: "ADMIN", PasswordHash: "x", TotpSecret: "y",
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("duplicate username should be unique_violation 23505, got %v", err)
		}
	})

	t.Run("sessions", func(t *testing.T) {
		u, err := q.GetUserByUsername(ctx, "admin")
		if err != nil {
			t.Fatalf("get user: %v", err)
		}

		live := "live-token-hash"
		if err := q.CreateSession(ctx, db.CreateSessionParams{
			TokenHash: live, UserID: u.ID, ExpiresAt: tsAt(time.Now().Add(time.Hour)),
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		sess, err := q.GetSession(ctx, live)
		if err != nil || sess.UserID != u.ID {
			t.Fatalf("GetSession live: got %+v, %v", sess, err)
		}
		if err := q.TouchSession(ctx, live); err != nil {
			t.Fatalf("TouchSession: %v", err)
		}

		// An expired session is invisible to GetSession (and swept).
		expired := "expired-token-hash"
		if err := q.CreateSession(ctx, db.CreateSessionParams{
			TokenHash: expired, UserID: u.ID, ExpiresAt: tsAt(time.Now().Add(-time.Hour)),
		}); err != nil {
			t.Fatalf("CreateSession expired: %v", err)
		}
		if _, err := q.GetSession(ctx, expired); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("expired session should be ErrNoRows, got %v", err)
		}
		purged, err := q.DeleteExpiredSessions(ctx)
		if err != nil || purged != 1 {
			t.Fatalf("DeleteExpiredSessions: purged %d, %v", purged, err)
		}

		// DeleteSession on an unknown token is a no-op, not an error.
		if err := q.DeleteSession(ctx, "nonexistent"); err != nil {
			t.Fatalf("DeleteSession unknown: %v", err)
		}
		// Revoking by user removes the live one (instant revocation, ADR-0003).
		if err := q.DeleteUserSessions(ctx, u.ID); err != nil {
			t.Fatalf("DeleteUserSessions: %v", err)
		}
		if _, err := q.GetSession(ctx, live); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("revoked session should be ErrNoRows, got %v", err)
		}
	})

	t.Run("audit log is ordered most-recent-first", func(t *testing.T) {
		if err := q.AppendAuditEntry(ctx, db.AppendAuditEntryParams{
			Actor: "admin", Action: "app.create", Target: "orkano-apps/web", Outcome: "success",
			Detail: []byte(`{"replicas":2}`),
		}); err != nil {
			t.Fatalf("AppendAuditEntry: %v", err)
		}
		// A nil detail is coalesced to '{}' so an audit write never fails on it.
		if err := q.AppendAuditEntry(ctx, db.AppendAuditEntryParams{
			Actor: "admin", Action: "secret.rotate", Target: "orkano-apps/web", Outcome: "success",
		}); err != nil {
			t.Fatalf("AppendAuditEntry nil detail: %v", err)
		}
		entries, err := q.ListAuditEntries(ctx, db.ListAuditEntriesParams{Limit: 10, Offset: 0})
		if err != nil {
			t.Fatalf("ListAuditEntries: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("want 2 audit entries, got %d", len(entries))
		}
		if entries[0].ID <= entries[1].ID {
			t.Fatalf("audit entries not most-recent-first: %d then %d", entries[0].ID, entries[1].ID)
		}
		if entries[0].Action != "secret.rotate" || len(entries[0].Detail) == 0 {
			t.Fatalf("nil detail should round-trip as '{}', got %+v", entries[0])
		}
	})

	t.Run("deploy history is filtered per app", func(t *testing.T) {
		for _, p := range []db.RecordDeployParams{
			{AppNamespace: "orkano-apps", AppName: "web", BuildName: "web-aaa", Image: "reg/web@sha256:aaa", Status: "Succeeded"},
			{AppNamespace: "orkano-apps", AppName: "web", BuildName: "web-bbb", Image: "reg/web@sha256:bbb", Status: "Succeeded"},
			{AppNamespace: "orkano-apps", AppName: "api", BuildName: "api-ccc", Image: "reg/api@sha256:ccc", Status: "Failed"},
		} {
			if _, err := q.RecordDeploy(ctx, p); err != nil {
				t.Fatalf("RecordDeploy %s: %v", p.AppName, err)
			}
		}
		deploys, err := q.ListAppDeploys(ctx, db.ListAppDeploysParams{
			AppNamespace: "orkano-apps", AppName: "web", Limit: 10, Offset: 0,
		})
		if err != nil {
			t.Fatalf("ListAppDeploys: %v", err)
		}
		if len(deploys) != 2 {
			t.Fatalf("want 2 deploys for web, got %d", len(deploys))
		}
		if deploys[0].BuildName != "web-bbb" {
			t.Fatalf("deploys not most-recent-first: %+v", deploys)
		}
	})

	t.Run("deleting a user cascades to its sessions", func(t *testing.T) {
		u, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: "throwaway", PasswordHash: "h", TotpSecret: "s",
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if err := q.CreateSession(ctx, db.CreateSessionParams{
			TokenHash: "cascade-token", UserID: u.ID, ExpiresAt: tsAt(time.Now().Add(time.Hour)),
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id = $1", u.ID); err != nil {
			t.Fatalf("delete user: %v", err)
		}
		if _, err := q.GetSession(ctx, "cascade-token"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("session should cascade-delete with its user, got %v", err)
		}
	})
}

// TestDashboardRoleBlastRadius proves the least-privilege grants of the
// orkano_dashboard role (migration 00004): full CRUD on its own account/session
// tables, append+read on deploy_history, and — the INV-08 guarantee — append+read
// but never UPDATE/DELETE on the audit log. It also has no reach into the webhook
// queue at all.
func TestDashboardRoleBlastRadius(t *testing.T) {
	dsn := startPostgres(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	// init sets these passwords at install via ALTER ROLE; the migration ships
	// only the privilege shape. Play that step here (the queue roles too, for the
	// cross-component isolation check at the end).
	for role, pw := range map[string]string{
		"orkano_dashboard":  "dash-pw",
		"orkano_receiver":   "recv-pw",
		"orkano_dispatcher": "disp-pw",
	} {
		if _, err := admin.Exec(ctx, "ALTER ROLE "+role+" WITH PASSWORD '"+pw+"'"); err != nil {
			t.Fatalf("set %s password: %v", role, err)
		}
	}

	dash := connectAs(t, dsn, "orkano_dashboard", "dash-pw")
	dq := db.New(dash)

	// Its own tables: full CRUD, exercised through the generated queries.
	u, err := dq.CreateUser(ctx, db.CreateUserParams{Username: "admin", PasswordHash: "h", TotpSecret: "s"})
	if err != nil {
		t.Fatalf("dashboard CreateUser should succeed: %v", err)
	}
	if err := dq.CreateSession(ctx, db.CreateSessionParams{
		TokenHash: "h1", UserID: u.ID, ExpiresAt: tsAt(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatalf("dashboard CreateSession should succeed: %v", err)
	}
	if _, err := dq.GetSession(ctx, "h1"); err != nil {
		t.Fatalf("dashboard GetSession should succeed: %v", err)
	}
	if _, err := dash.Exec(ctx, "UPDATE users SET updated_at = now() WHERE id = $1", u.ID); err != nil {
		t.Fatalf("dashboard UPDATE users should succeed: %v", err)
	}
	if err := dq.DeleteSession(ctx, "h1"); err != nil {
		t.Fatalf("dashboard DeleteSession should succeed: %v", err)
	}

	// audit_log: append + read, never rewrite or erase (INV-08).
	if err := dq.AppendAuditEntry(ctx, db.AppendAuditEntryParams{
		Actor: "admin", Action: "app.delete", Target: "orkano-apps/web", Outcome: "success",
	}); err != nil {
		t.Fatalf("dashboard AppendAuditEntry should succeed: %v", err)
	}
	if entries, err := dq.ListAuditEntries(ctx, db.ListAuditEntriesParams{Limit: 10}); err != nil || len(entries) != 1 {
		t.Fatalf("dashboard ListAuditEntries: got %d, %v", len(entries), err)
	}
	_, err = dash.Exec(ctx, "UPDATE audit_log SET actor = 'tamper'")
	assertDenied(t, "dashboard UPDATE audit_log", err)
	_, err = dash.Exec(ctx, "DELETE FROM audit_log")
	assertDenied(t, "dashboard DELETE audit_log", err)
	_, err = dash.Exec(ctx, "TRUNCATE audit_log")
	assertDenied(t, "dashboard TRUNCATE audit_log", err)

	// deploy_history: append + read, never UPDATE/DELETE/TRUNCATE.
	if _, err := dq.RecordDeploy(ctx, db.RecordDeployParams{
		AppNamespace: "orkano-apps", AppName: "web", Image: "reg/web@sha256:x", Status: "Succeeded",
	}); err != nil {
		t.Fatalf("dashboard RecordDeploy should succeed: %v", err)
	}
	if deploys, err := dq.ListAppDeploys(ctx, db.ListAppDeploysParams{
		AppNamespace: "orkano-apps", AppName: "web", Limit: 10,
	}); err != nil || len(deploys) != 1 {
		t.Fatalf("dashboard ListAppDeploys should succeed: got %d, %v", len(deploys), err)
	}
	_, err = dash.Exec(ctx, "UPDATE deploy_history SET status = 'x'")
	assertDenied(t, "dashboard UPDATE deploy_history", err)
	_, err = dash.Exec(ctx, "DELETE FROM deploy_history")
	assertDenied(t, "dashboard DELETE deploy_history", err)
	_, err = dash.Exec(ctx, "TRUNCATE deploy_history")
	assertDenied(t, "dashboard TRUNCATE deploy_history", err)

	// The D in "full CRUD on users", as the dashboard role (the cascade above ran
	// as the superuser).
	if _, err := dash.Exec(ctx, "DELETE FROM users WHERE id = $1", u.ID); err != nil {
		t.Fatalf("dashboard DELETE users should succeed: %v", err)
	}

	// The dashboard role holds nothing on the webhook queue — it can neither read
	// nor ring the doorbell (cross-component blast-radius).
	_, err = dash.Exec(ctx, "SELECT delivery_id FROM webhook_deliveries")
	assertDenied(t, "dashboard SELECT webhook_deliveries", err)
	_, err = dash.Exec(ctx, "INSERT INTO webhook_deliveries (delivery_id, repo, event_type) VALUES ('x','y','push')")
	assertDenied(t, "dashboard INSERT webhook_deliveries", err)

	// The reverse direction: the internet-facing receiver and the dispatcher hold
	// nothing on the dashboard's account store — a DB compromise of the doorbell
	// yields no users/sessions/audit.
	recv := connectAs(t, dsn, "orkano_receiver", "recv-pw")
	_, err = recv.Exec(ctx, "SELECT id FROM users")
	assertDenied(t, "receiver SELECT users", err)
	disp := connectAs(t, dsn, "orkano_dispatcher", "disp-pw")
	_, err = disp.Exec(ctx, "SELECT id FROM users")
	assertDenied(t, "dispatcher SELECT users", err)
}

// TestDashboardSchemaHasNoSecretValueColumns pins the exact column set of each
// dashboard table (INV-03): a future change cannot quietly add a column that
// stores a user-app secret VALUE. The auth-material columns (password_hash,
// totp_secret) are the dashboard's OWN credential store per ADR-0003, a different
// category from the user-app secrets INV-03 protects — those live only in
// Kubernetes Secrets.
func TestDashboardSchemaHasNoSecretValueColumns(t *testing.T) {
	dsn := startPostgres(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	want := []struct {
		table string
		cols  []string
	}{
		{"users", []string{"id", "username", "password_hash", "totp_secret", "totp_confirmed_at", "created_at", "updated_at"}},
		{"sessions", []string{"token_hash", "user_id", "created_at", "expires_at", "last_used_at"}},
		{"audit_log", []string{"id", "occurred_at", "actor", "action", "target", "outcome", "detail"}},
		{"deploy_history", []string{"id", "occurred_at", "app_namespace", "app_name", "build_name", "image", "status"}},
	}
	for _, tc := range want {
		rows, err := pool.Query(ctx,
			`SELECT column_name FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = $1
			 ORDER BY ordinal_position`, tc.table)
		if err != nil {
			t.Errorf("describe %s: %v", tc.table, err)
			continue
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Errorf("collect %s: %v", tc.table, err)
			continue
		}
		if !slices.Equal(got, tc.cols) {
			t.Errorf("%s columns drifted: got %v, want %v", tc.table, got, tc.cols)
		}
	}
}
