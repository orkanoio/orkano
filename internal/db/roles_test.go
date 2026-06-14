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
	if err == nil {
		t.Fatalf("%s: expected permission denied, got success", op)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("%s: expected SQLSTATE 42501 (insufficient_privilege), got: %v", op, err)
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

		// The one thing the receiver may do (also proves the GENERATED ALWAYS
		// identity column needs no sequence grant — INSERT is the only grant).
		if _, err := recv.Exec(ctx,
			"INSERT INTO webhook_deliveries (delivery_id, repo, event_type) VALUES ($1, $2, $3)",
			"recv-1", "orkanoio/orkano", "push"); err != nil {
			t.Fatalf("receiver INSERT should succeed: %v", err)
		}

		// Everything that would let it read or drain the queue is denied.
		_, err := recv.Exec(ctx, "SELECT delivery_id FROM webhook_deliveries")
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

		// The consume path: read + lock, then remove.
		if _, err := disp.Exec(ctx, "SELECT delivery_id FROM webhook_deliveries FOR UPDATE SKIP LOCKED"); err != nil {
			t.Fatalf("dispatcher SELECT FOR UPDATE should succeed: %v", err)
		}
		if _, err := disp.Exec(ctx, "DELETE FROM webhook_deliveries WHERE delivery_id = $1", "disp-1"); err != nil {
			t.Fatalf("dispatcher DELETE should succeed: %v", err)
		}

		// The dispatcher is a consumer, never a producer.
		_, err := disp.Exec(ctx,
			"INSERT INTO webhook_deliveries (delivery_id, repo, event_type) VALUES ($1, $2, $3)",
			"disp-2", "orkanoio/orkano", "push")
		assertDenied(t, "dispatcher INSERT", err)

		// …and it consumes row-at-a-time: no TRUNCATE means it can never drain
		// the whole queue in one statement (guards against a future stray grant).
		_, err = disp.Exec(ctx, "TRUNCATE webhook_deliveries")
		assertDenied(t, "dispatcher TRUNCATE", err)
	})
}
