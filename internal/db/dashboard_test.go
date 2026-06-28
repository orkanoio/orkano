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

// TestDashboardAuthRoundTrip exercises the M2.3 bootstrap-auth queries (migration
// 00005): account lockout state, TOTP confirmation, abandoned-enrollment cleanup,
// single-use recovery codes, and the session step-up re-auth marker.
func TestDashboardAuthRoundTrip(t *testing.T) {
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

	// Each sub-test starts from an empty users table: the single-confirmed-admin
	// partial unique index (migration 00005) forbids more than one confirmed admin
	// across this shared container, so sub-tests must not accumulate confirmed users.
	reset := func(t *testing.T) {
		if _, err := pool.Exec(ctx, "DELETE FROM users"); err != nil {
			t.Fatalf("reset users: %v", err)
		}
	}

	t.Run("lockout counters", func(t *testing.T) {
		reset(t)
		u, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: "lockme", PasswordHash: "h", TotpSecret: "s",
			TotpConfirmedAt: tsAt(time.Now().UTC()),
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		// A fresh account starts unlocked with a zero counter.
		if u.FailedLogins != 0 || u.LockedUntil.Valid {
			t.Fatalf("fresh user should be unlocked: %+v", u)
		}

		n1, err := q.IncrementFailedLogins(ctx, u.ID)
		if err != nil || n1 != 1 {
			t.Fatalf("IncrementFailedLogins #1: got %d, %v", n1, err)
		}
		n2, err := q.IncrementFailedLogins(ctx, u.ID)
		if err != nil || n2 != 2 {
			t.Fatalf("IncrementFailedLogins #2: got %d, %v", n2, err)
		}

		// Postgres timestamptz is microsecond precision, so compare against the
		// value truncated to microseconds (a raw nanosecond Equal would flake).
		until := time.Now().Add(15 * time.Minute).UTC().Truncate(time.Microsecond)
		if err := q.LockUser(ctx, db.LockUserParams{UserID: u.ID, LockedUntil: tsAt(until)}); err != nil {
			t.Fatalf("LockUser: %v", err)
		}
		locked, err := q.GetUserByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("GetUserByID: %v", err)
		}
		if locked.FailedLogins != 2 || !locked.LockedUntil.Valid {
			t.Fatalf("expected locked user with 2 failures: %+v", locked)
		}
		if !locked.LockedUntil.Time.Equal(until) {
			t.Fatalf("locked_until round-trip: got %v want %v", locked.LockedUntil.Time, until)
		}

		// A successful login clears both counters.
		if err := q.ResetFailedLogins(ctx, u.ID); err != nil {
			t.Fatalf("ResetFailedLogins: %v", err)
		}
		cleared, err := q.GetUserByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("GetUserByID after reset: %v", err)
		}
		if cleared.FailedLogins != 0 || cleared.LockedUntil.Valid {
			t.Fatalf("expected cleared lockout: %+v", cleared)
		}
	})

	t.Run("totp confirmation and abandoned-enrollment cleanup", func(t *testing.T) {
		reset(t)
		// Start an enrollment whose second factor is not yet confirmed.
		pending, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: "pending", PasswordHash: "h", TotpSecret: "s",
		})
		if err != nil {
			t.Fatalf("CreateUser pending: %v", err)
		}
		if pending.TotpConfirmedAt.Valid {
			t.Fatalf("new enrollment should be unconfirmed: %+v", pending)
		}
		// No confirmed admin exists yet after the reset.
		before, err := q.CountConfirmedAdmins(ctx)
		if err != nil {
			t.Fatalf("CountConfirmedAdmins: %v", err)
		}

		if err := q.ConfirmUserTOTP(ctx, pending.ID); err != nil {
			t.Fatalf("ConfirmUserTOTP: %v", err)
		}
		confirmed, err := q.GetUserByID(ctx, pending.ID)
		if err != nil {
			t.Fatalf("GetUserByID confirmed: %v", err)
		}
		if !confirmed.TotpConfirmedAt.Valid {
			t.Fatalf("ConfirmUserTOTP should flip totp_confirmed_at: %+v", confirmed)
		}
		after, err := q.CountConfirmedAdmins(ctx)
		if err != nil || after != before+1 {
			t.Fatalf("CountConfirmedAdmins after confirm: got %d, want %d, %v", after, before+1, err)
		}

		// A second, abandoned enrollment, then a cleanup before a fresh redeem.
		if _, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: "abandoned", PasswordHash: "h", TotpSecret: "s",
		}); err != nil {
			t.Fatalf("CreateUser abandoned: %v", err)
		}
		if err := q.DeleteUnconfirmedUsers(ctx); err != nil {
			t.Fatalf("DeleteUnconfirmedUsers: %v", err)
		}
		// Only unconfirmed rows are removed; confirmed admins survive.
		survivors, err := q.CountConfirmedAdmins(ctx)
		if err != nil || survivors != after {
			t.Fatalf("DeleteUnconfirmedUsers removed a confirmed admin: got %d, want %d, %v", survivors, after, err)
		}
		if _, err := q.GetUserByUsername(ctx, "abandoned"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("abandoned enrollment should be gone, got %v", err)
		}
	})

	t.Run("recovery codes are single-use", func(t *testing.T) {
		reset(t)
		u, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: "recovery", PasswordHash: "h", TotpSecret: "s",
			TotpConfirmedAt: tsAt(time.Now().UTC()),
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		const hash = "sha256-of-a-recovery-code"
		if err := q.InsertRecoveryCode(ctx, db.InsertRecoveryCodeParams{UserID: u.ID, CodeHash: hash}); err != nil {
			t.Fatalf("InsertRecoveryCode: %v", err)
		}
		if err := q.InsertRecoveryCode(ctx, db.InsertRecoveryCodeParams{UserID: u.ID, CodeHash: "another-hash"}); err != nil {
			t.Fatalf("InsertRecoveryCode #2: %v", err)
		}
		if n, err := q.CountUnusedRecoveryCodes(ctx, u.ID); err != nil || n != 2 {
			t.Fatalf("CountUnusedRecoveryCodes: got %d, %v", n, err)
		}

		id, err := q.ConsumeRecoveryCode(ctx, db.ConsumeRecoveryCodeParams{UserID: u.ID, CodeHash: hash})
		if err != nil || id == 0 {
			t.Fatalf("ConsumeRecoveryCode: got id %d, %v", id, err)
		}
		// Re-presenting a spent code returns no rows (single-use).
		if _, err := q.ConsumeRecoveryCode(ctx, db.ConsumeRecoveryCodeParams{UserID: u.ID, CodeHash: hash}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("second consume should be ErrNoRows, got %v", err)
		}
		// An unknown code is also no rows, never a different error.
		if _, err := q.ConsumeRecoveryCode(ctx, db.ConsumeRecoveryCodeParams{UserID: u.ID, CodeHash: "never-issued"}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unknown code should be ErrNoRows, got %v", err)
		}
		if n, err := q.CountUnusedRecoveryCodes(ctx, u.ID); err != nil || n != 1 {
			t.Fatalf("CountUnusedRecoveryCodes after consume: got %d, %v", n, err)
		}
	})

	t.Run("session step-up reauth marker", func(t *testing.T) {
		reset(t)
		u, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: "stepup", PasswordHash: "h", TotpSecret: "s",
			TotpConfirmedAt: tsAt(time.Now().UTC()),
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		const tok = "stepup-token-hash"
		if err := q.CreateSession(ctx, db.CreateSessionParams{
			TokenHash: tok, UserID: u.ID, ExpiresAt: tsAt(time.Now().Add(time.Hour)),
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		// A fresh session has no step-up yet.
		sess, err := q.GetSession(ctx, tok)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if sess.ReauthAt.Valid {
			t.Fatalf("fresh session should have no reauth_at: %+v", sess)
		}
		if err := q.MarkSessionReauth(ctx, tok); err != nil {
			t.Fatalf("MarkSessionReauth: %v", err)
		}
		stepped, err := q.GetSession(ctx, tok)
		if err != nil {
			t.Fatalf("GetSession after step-up: %v", err)
		}
		if !stepped.ReauthAt.Valid {
			t.Fatalf("MarkSessionReauth should set reauth_at: %+v", stepped)
		}
	})

	t.Run("recovery codes cascade on user delete", func(t *testing.T) {
		reset(t)
		u, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: "cascade-rc", PasswordHash: "h", TotpSecret: "s",
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if err := q.InsertRecoveryCode(ctx, db.InsertRecoveryCodeParams{UserID: u.ID, CodeHash: "cascade-rc-hash"}); err != nil {
			t.Fatalf("InsertRecoveryCode: %v", err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id = $1", u.ID); err != nil {
			t.Fatalf("delete user: %v", err)
		}
		// The recovery code is gone with its user (ON DELETE CASCADE).
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM recovery_codes WHERE user_id = $1", u.ID).Scan(&n); err != nil {
			t.Fatalf("count recovery_codes: %v", err)
		}
		if n != 0 {
			t.Fatalf("recovery codes should cascade-delete with the user, got %d remaining", n)
		}
	})

	t.Run("recovery code (user_id, code_hash) is unique", func(t *testing.T) {
		reset(t)
		u, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: "dup-rc", PasswordHash: "h", TotpSecret: "s",
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if err := q.InsertRecoveryCode(ctx, db.InsertRecoveryCodeParams{UserID: u.ID, CodeHash: "same-hash"}); err != nil {
			t.Fatalf("InsertRecoveryCode #1: %v", err)
		}
		err = q.InsertRecoveryCode(ctx, db.InsertRecoveryCodeParams{UserID: u.ID, CodeHash: "same-hash"})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("duplicate (user_id, code_hash) should be 23505, got %v", err)
		}
	})

	t.Run("at most one confirmed admin", func(t *testing.T) {
		reset(t)
		if _, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: "first-admin", PasswordHash: "h", TotpSecret: "s",
			TotpConfirmedAt: tsAt(time.Now().UTC()),
		}); err != nil {
			t.Fatalf("CreateUser first confirmed: %v", err)
		}
		// A SECOND confirmed admin trips the single-confirmed-admin partial unique
		// index (migration 00005) — the atomic backstop to a concurrent redeem.
		_, err := q.CreateUser(ctx, db.CreateUserParams{
			Username: "second-admin", PasswordHash: "h", TotpSecret: "s",
			TotpConfirmedAt: tsAt(time.Now().UTC()),
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("a second confirmed admin should be 23505, got %v", err)
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

	// recovery_codes (migration 00005): full CRUD — INSERT, SELECT, UPDATE (mark a
	// code used) and DELETE (regeneration).
	if err := dq.InsertRecoveryCode(ctx, db.InsertRecoveryCodeParams{UserID: u.ID, CodeHash: "rc-hash"}); err != nil {
		t.Fatalf("dashboard InsertRecoveryCode should succeed: %v", err)
	}
	if n, err := dq.CountUnusedRecoveryCodes(ctx, u.ID); err != nil || n != 1 {
		t.Fatalf("dashboard CountUnusedRecoveryCodes should succeed: got %d, %v", n, err)
	}
	if _, err := dq.ConsumeRecoveryCode(ctx, db.ConsumeRecoveryCodeParams{UserID: u.ID, CodeHash: "rc-hash"}); err != nil {
		t.Fatalf("dashboard ConsumeRecoveryCode (UPDATE) should succeed: %v", err)
	}
	if _, err := dash.Exec(ctx, "DELETE FROM recovery_codes WHERE user_id = $1", u.ID); err != nil {
		t.Fatalf("dashboard DELETE recovery_codes should succeed: %v", err)
	}
	// TRUNCATE is owner-only DDL — denied for the dashboard role (matches the
	// audit_log/deploy_history TRUNCATE checks above).
	_, err = dash.Exec(ctx, "TRUNCATE recovery_codes")
	assertDenied(t, "dashboard TRUNCATE recovery_codes", err)

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
	_, err = recv.Exec(ctx, "SELECT id FROM recovery_codes")
	assertDenied(t, "receiver SELECT recovery_codes", err)
	disp := connectAs(t, dsn, "orkano_dispatcher", "disp-pw")
	_, err = disp.Exec(ctx, "SELECT id FROM users")
	assertDenied(t, "dispatcher SELECT users", err)
	_, err = disp.Exec(ctx, "SELECT id FROM recovery_codes")
	assertDenied(t, "dispatcher SELECT recovery_codes", err)
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
		{"users", []string{"id", "username", "password_hash", "totp_secret", "totp_confirmed_at", "created_at", "updated_at", "failed_logins", "locked_until"}},
		{"sessions", []string{"token_hash", "user_id", "created_at", "expires_at", "last_used_at", "reauth_at"}},
		{"audit_log", []string{"id", "occurred_at", "actor", "action", "target", "outcome", "detail"}},
		{"deploy_history", []string{"id", "occurred_at", "app_namespace", "app_name", "build_name", "image", "status"}},
		// recovery_codes (00005): hashed (one-way) codes only, never plaintext.
		{"recovery_codes", []string{"id", "user_id", "code_hash", "used_at", "created_at"}},
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
