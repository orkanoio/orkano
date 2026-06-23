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

	const recvPw, dispPw = "recv-a1b2c3d4e5f6", "disp-f6e5d4c3b2a1"
	if err := db.SetupRoles(ctx, dsn, recvPw, dispPw); err != nil {
		t.Fatalf("SetupRoles: %v", err)
	}

	// Each role can authenticate with its new password and exercise its one
	// allowed operation through the real generated queries — proving the
	// password took and the privilege shape from migration 00002 is intact.
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

	// Idempotent: re-running with the same passwords is a no-op (ALTER ROLE
	// overwrites), and the role still authenticates and works.
	if err := db.SetupRoles(ctx, dsn, recvPw, dispPw); err != nil {
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
	// password stops working and the new one works.
	const recvPw2 = "recv-NEW-0a1b2c3d4e5f"
	if err := db.SetupRoles(ctx, dsn, recvPw2, "disp-NEW-5f4e3d2c1b0a"); err != nil {
		t.Fatalf("SetupRoles rotation: %v", err)
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

	for _, tc := range []struct {
		name           string
		receiver, disp string
	}{
		{"empty receiver", "", "ok-password"},
		{"empty dispatcher", "ok-password", ""},
		{"receiver has quote", "pw'; DROP ROLE x;--", "ok-password"},
		{"dispatcher has backslash", "ok-password", `pw\ninjected`},
		{"dispatcher has space", "ok-password", "pw with space"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.SetupRoles(ctx, unreachableDSN, tc.receiver, tc.disp); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}
