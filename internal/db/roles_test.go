package db_test

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orkanoio/orkano/internal/db"
)

// connectAs opens a pool authenticated as a specific role by swapping the
// credentials in the superuser dsn. pgxpool connects lazily, so an auth or
// permission failure surfaces on the first query, not here.
func connectAs(t *testing.T, dsn, user, password string) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(user, password)
	pool, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", user, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// assertDenied fails unless err is a Postgres insufficient_privilege error
// (SQLSTATE 42501) — proving the operation was rejected by RBAC, not by some
// unrelated failure.
func assertDenied(t *testing.T, op string, err error) {
	t.Helper()
	assertSQLState(t, op, err, "42501")
}

func assertSQLState(t *testing.T, op string, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected SQLSTATE %s, got success", op, code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("%s: expected SQLSTATE %s, got: %v", op, code, err)
	}
}

// TestQueueRolesBlastRadius proves the least-privilege grants from migration
// 00002: the receiver can only enqueue, the dispatcher can only consume.
func TestQueueRolesBlastRadius(t *testing.T) {
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
	// only the privilege shape. Play that step here.
	for role, pw := range map[string]string{"orkano_receiver": "recv-pw", "orkano_dispatcher": "disp-pw"} {
		if _, err := admin.Exec(ctx, "ALTER ROLE "+role+" WITH PASSWORD '"+pw+"'"); err != nil {
			t.Fatalf("set password for %s: %v", role, err)
		}
	}

	t.Run("receiver can only INSERT", func(t *testing.T) {
		recv := connectAs(t, dsn, "orkano_receiver", "recv-pw")

		// The one thing the receiver may do — exercised through the ACTUAL
		// generated query, not a hand-written INSERT: the query's bare
		// ON CONFLICT DO NOTHING must run under INSERT-only (a named-arbiter
		// ON CONFLICT would infer the index and need SELECT on delivery_id,
		// failing here — this is the regression guard for the receiver's
		// enqueue path). Also proves the GENERATED ALWAYS identity column needs
		// no sequence grant — INSERT is the only grant.
		if _, err := db.New(recv).EnqueueDelivery(ctx, db.EnqueueDeliveryParams{
			DeliveryID: "recv-1", Repo: "orkanoio/orkano", EventType: "push",
		}); err != nil {
			t.Fatalf("receiver EnqueueDelivery should succeed: %v", err)
		}
		_, err := recv.Exec(ctx, "INSERT INTO webhook_deliveries (delivery_id, repo, event_type, app_name) VALUES ('manual-11111111111111111111111111111111', 'orkanoio/orkano', 'manual', 'target')")
		assertDenied(t, "receiver app-scoped INSERT", err)
		_, err = recv.Exec(ctx, "INSERT INTO webhook_deliveries (delivery_id, repo, event_type) VALUES ('manual-22222222222222222222222222222222', 'orkanoio/orkano', 'manual')")
		assertSQLState(t, "receiver forged manual event", err, "23514")

		// Everything that would let it read or drain the queue is denied.
		_, err = recv.Exec(ctx, "SELECT delivery_id FROM webhook_deliveries")
		assertDenied(t, "receiver SELECT", err)
		_, err = recv.Exec(ctx, "UPDATE webhook_deliveries SET repo = 'x'")
		assertDenied(t, "receiver UPDATE", err)
		_, err = recv.Exec(ctx, "DELETE FROM webhook_deliveries")
		assertDenied(t, "receiver DELETE", err)
		_, err = recv.Exec(ctx, "TRUNCATE webhook_deliveries")
		assertDenied(t, "receiver TRUNCATE", err)
	})

	t.Run("dispatcher consumes but cannot enqueue", func(t *testing.T) {
		disp := connectAs(t, dsn, "orkano_dispatcher", "disp-pw")

		// Seed a row as admin so the dispatcher has something to consume.
		if _, err := admin.Exec(ctx,
			"INSERT INTO webhook_deliveries (delivery_id, repo, event_type) VALUES ($1, $2, $3)",
			"disp-1", "orkanoio/orkano", "push"); err != nil {
			t.Fatalf("seed row: %v", err)
		}

		// The consume path, through the ACTUAL generated queries (not hand-written
		// SQL): ClaimDelivery's FOR UPDATE needs the role's UPDATE grant and
		// DeleteDelivery its DELETE grant, run in one transaction exactly as the
		// dispatcher consumes. This is the generated-code regression guard, the
		// mirror of the receiver branch exercising EnqueueDelivery above.
		tx, err := disp.Begin(ctx)
		if err != nil {
			t.Fatalf("dispatcher begin: %v", err)
		}
		row, err := db.New(tx).ClaimDelivery(ctx)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("dispatcher ClaimDelivery should succeed: %v", err)
		}
		if err := db.New(tx).DeleteDelivery(ctx, row.ID); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("dispatcher DeleteDelivery should succeed: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("dispatcher commit should succeed: %v", err)
		}

		// The dispatcher is a consumer, never a producer — even through the
		// generated enqueue path.
		_, err = db.New(disp).EnqueueDelivery(ctx, db.EnqueueDeliveryParams{
			DeliveryID: "disp-2", Repo: "orkanoio/orkano", EventType: "push",
		})
		assertDenied(t, "dispatcher EnqueueDelivery", err)

		// …and it consumes row-at-a-time: no TRUNCATE means it can never drain
		// the whole queue in one statement (guards against a future stray grant).
		_, err = disp.Exec(ctx, "TRUNCATE webhook_deliveries")
		assertDenied(t, "dispatcher TRUNCATE", err)
	})
}
