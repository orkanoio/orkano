package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/orkanoio/orkano/internal/db"
)

// assertAuthFailed fails unless err is a Postgres authentication error — the
// password, not an always-open role, is what gates access. 28P01 is
// invalid_password; 28000 is invalid_authorization_specification (either can
// surface depending on the server's pg_hba/auth method).
func assertAuthFailed(t *testing.T, op string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an authentication failure, got success", op)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != "28P01" && pgErr.Code != "28000") {
		t.Fatalf("%s: expected SQLSTATE 28P01/28000 (auth failure), got: %v", op, err)
	}
}

// TestSetupRoles proves the install-time role-password step: after Migrate, the
// passwordless roles from migration 00002 become usable login roles with the
// install-generated passwords, a same-password re-run is idempotent, and a
// later run with fresh passwords rotates the credentials (old rejected, new
// works) — the property a second install relies on.
func TestSetupRoles(t *testing.T) {
	dsn := startPostgres(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const recvPw, dispPw, dashPw = "recv-a1b2c3d4e5f6", "disp-f6e5d4c3b2a1", "dash-0a1b2c3d4e5f"
	pw := db.RolePasswords{Receiver: recvPw, Dispatcher: dispPw, Dashboard: dashPw}
	if err := db.SetupRoles(ctx, dsn, pw); err != nil {
		t.Fatalf("SetupRoles: %v", err)
	}

	// Each role can authenticate with its new password and exercise its one
	// allowed operation through the real generated queries — proving the
	// password took and the privilege shape from migrations 00002/00004 is intact.
	recv := connectAs(t, dsn, db.ReceiverRole, recvPw)
	if _, err := db.New(recv).EnqueueDelivery(ctx, db.EnqueueDeliveryParams{
		DeliveryID: "setup-1", Repo: "orkanoio/orkano", EventType: "push",
	}); err != nil {
		t.Fatalf("receiver EnqueueDelivery after SetupRoles: %v", err)
	}
	disp := connectAs(t, dsn, db.DispatcherRole, dispPw)
	// ClaimDelivery's FOR UPDATE needs the dispatcher's UPDATE grant; an empty
	// queue returns ErrNoRows, which (unlike 42501) proves the grant is present.
	if _, err := db.New(disp).ClaimDelivery(ctx); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("dispatcher ClaimDelivery after SetupRoles: %v", err)
	}
	// CountUsers needs the dashboard role's SELECT on users (migration 00004); a
	// fresh schema returns 0, proving the password took and the grant is present.
	dash := connectAs(t, dsn, db.DashboardRole, dashPw)
	if _, err := db.New(dash).CountUsers(ctx); err != nil {
		t.Fatalf("dashboard CountUsers after SetupRoles: %v", err)
	}

	// Idempotent: re-running with the same passwords is a no-op (ALTER ROLE
	// overwrites), and the role still authenticates and works.
	if err := db.SetupRoles(ctx, dsn, pw); err != nil {
		t.Fatalf("SetupRoles re-run: %v", err)
	}
	again := connectAs(t, dsn, db.ReceiverRole, recvPw)
	if _, err := db.New(again).EnqueueDelivery(ctx, db.EnqueueDeliveryParams{
		DeliveryID: "setup-2", Repo: "orkanoio/orkano", EventType: "push",
	}); err != nil {
		t.Fatalf("receiver usable after SetupRoles re-run: %v", err)
	}

	// A wrong password is rejected at the auth layer — the password is doing
	// the work, not an always-open role.
	wrong := connectAs(t, dsn, db.ReceiverRole, "not-the-password")
	_, err := wrong.Exec(ctx, "SELECT 1")
	assertAuthFailed(t, "wrong password", err)

	// Rotation: a fresh install with new passwords takes effect — the old
	// password stops working and the new one works. Checked for the receiver and
	// the dashboard (added in 00004) so a role-specific ALTER ROLE no-op or
	// ordering bug in the loop would surface, not just for the first role.
	const recvPw2, dashPw2 = "recv-NEW-0a1b2c3d4e5f", "dash-NEW-9f8e7d6c5b4a"
	if err := db.SetupRoles(ctx, dsn, db.RolePasswords{
		Receiver: recvPw2, Dispatcher: "disp-NEW-5f4e3d2c1b0a", Dashboard: dashPw2,
	}); err != nil {
		t.Fatalf("SetupRoles rotation: %v", err)
	}
	oldDash := connectAs(t, dsn, db.DashboardRole, dashPw)
	if _, err := oldDash.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatal("old dashboard password should be rejected after rotation")
	} else {
		assertAuthFailed(t, "old dashboard password after rotation", err)
	}
	rotatedDash := connectAs(t, dsn, db.DashboardRole, dashPw2)
	if _, err := db.New(rotatedDash).CountUsers(ctx); err != nil {
		t.Fatalf("dashboard usable after rotation: %v", err)
	}
	old := connectAs(t, dsn, db.ReceiverRole, recvPw)
	_, err = old.Exec(ctx, "SELECT 1")
	assertAuthFailed(t, "old password after rotation", err)
	rotated := connectAs(t, dsn, db.ReceiverRole, recvPw2)
	if _, err := db.New(rotated).EnqueueDelivery(ctx, db.EnqueueDeliveryParams{
		DeliveryID: "setup-3", Repo: "orkanoio/orkano", EventType: "push",
	}); err != nil {
		t.Fatalf("receiver usable after rotation: %v", err)
	}
}

// TestSetupRolesValidation proves a password with no value or with SQL-unsafe
// characters is rejected before any database connection is attempted — the
// inline ALTER ROLE statements can never carry an injectable value.
func TestSetupRolesValidation(t *testing.T) {
	ctx := context.Background()
	const unreachableDSN = "postgres://unused:unused@127.0.0.1:1/none"

	const validPw = "ok-password"
	for _, tc := range []struct {
		name string
		pw   db.RolePasswords
	}{
		{"empty receiver", db.RolePasswords{Receiver: "", Dispatcher: validPw, Dashboard: validPw}},
		{"empty dispatcher", db.RolePasswords{Receiver: validPw, Dispatcher: "", Dashboard: validPw}},
		{"empty dashboard", db.RolePasswords{Receiver: validPw, Dispatcher: validPw, Dashboard: ""}},
		{"receiver has quote", db.RolePasswords{Receiver: "pw'; DROP ROLE x;--", Dispatcher: validPw, Dashboard: validPw}},
		{"dispatcher has backslash", db.RolePasswords{Receiver: validPw, Dispatcher: `pw\ninjected`, Dashboard: validPw}},
		{"dispatcher has space", db.RolePasswords{Receiver: validPw, Dispatcher: "pw with space", Dashboard: validPw}},
		{"dashboard has quote", db.RolePasswords{Receiver: validPw, Dispatcher: validPw, Dashboard: "pw'--"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.SetupRoles(ctx, unreachableDSN, tc.pw); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}
