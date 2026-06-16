package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orkanoio/orkano/internal/db"
)

// TestDeliveryQueueConsume exercises the SEMANTICS of the dispatcher's consume
// queries (ClaimDelivery / DeleteDelivery) against a real Postgres: FIFO order,
// the FOR UPDATE SKIP LOCKED contract, and the empty-queue signal. The proof
// that the GENERATED queries run under the least-privilege orkano_dispatcher
// role lives with the other role boundaries in roles_test.go.
func TestDeliveryQueueConsume(t *testing.T) {
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

	seed := func(t *testing.T, deliveryID, repo string) {
		t.Helper()
		if _, err := admin.Exec(ctx,
			"INSERT INTO webhook_deliveries (delivery_id, repo, event_type) VALUES ($1, $2, 'push')",
			deliveryID, repo); err != nil {
			t.Fatalf("seed %s: %v", deliveryID, err)
		}
	}
	reset := func(t *testing.T) {
		t.Helper()
		if _, err := admin.Exec(ctx, "TRUNCATE webhook_deliveries"); err != nil {
			t.Fatalf("truncate: %v", err)
		}
	}

	t.Run("claims oldest first, then deletes", func(t *testing.T) {
		reset(t)
		seed(t, "first", "orkanoio/a")
		seed(t, "second", "orkanoio/b")

		// Each claim runs in its own transaction: FOR UPDATE holds the lock only
		// until COMMIT, mirroring how the dispatcher acks one delivery at a time.
		claim := func() db.ClaimDeliveryRow {
			tx, err := admin.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			row, err := db.New(tx).ClaimDelivery(ctx)
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if err := db.New(tx).DeleteDelivery(ctx, row.ID); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			return row
		}

		if got := claim(); got.DeliveryID != "first" {
			t.Fatalf("first claim = %q, want first (lowest id wins)", got.DeliveryID)
		}
		if got := claim(); got.DeliveryID != "second" {
			t.Fatalf("second claim = %q, want second", got.DeliveryID)
		}
	})

	t.Run("empty queue signals ErrNoRows", func(t *testing.T) {
		reset(t)
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := db.New(tx).ClaimDelivery(ctx); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("claim on empty queue: got %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("SKIP LOCKED hands a second claimer the next row", func(t *testing.T) {
		reset(t)
		seed(t, "locked", "orkanoio/a")
		seed(t, "free", "orkanoio/b")

		// tx1 claims and HOLDS the first row's lock (no commit yet).
		tx1, err := admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx1: %v", err)
		}
		defer func() { _ = tx1.Rollback(ctx) }()
		first, err := db.New(tx1).ClaimDelivery(ctx)
		if err != nil {
			t.Fatalf("tx1 claim: %v", err)
		}
		if first.DeliveryID != "locked" {
			t.Fatalf("tx1 claimed %q, want locked", first.DeliveryID)
		}

		// tx2 must SKIP the row tx1 holds and get the next one — never block,
		// never re-hand the same row. A short deadline turns a regression that
		// dropped SKIP LOCKED (a plain FOR UPDATE would block on tx1's lock)
		// into a fast, legible failure instead of a hang until the test timeout.
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		tx2, err := admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx2: %v", err)
		}
		defer func() { _ = tx2.Rollback(ctx) }()
		second, err := db.New(tx2).ClaimDelivery(claimCtx)
		if err != nil {
			t.Fatalf("tx2 claim: %v (a deadline here means FOR UPDATE blocked — is SKIP LOCKED missing?)", err)
		}
		if second.DeliveryID != "free" {
			t.Fatalf("tx2 claimed %q, want free (the unlocked row)", second.DeliveryID)
		}
	})
}
