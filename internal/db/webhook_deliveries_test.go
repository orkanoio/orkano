package db_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orkanoio/orkano/internal/db"
)

// newQueue starts one Postgres container, applies the migrations, and returns a
// query handle plus the raw pool and dsn for assertions the generated API does
// not cover.
func newQueue(t *testing.T) (*db.Queries, *pgxpool.Pool, string) {
	t.Helper()
	dsn := startPostgres(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return db.New(pool), pool, dsn
}

func TestWebhookQueue(t *testing.T) {
	q, pool, dsn := newQueue(t)
	ctx := context.Background()

	reset := func(t *testing.T) {
		t.Helper()
		if _, err := pool.Exec(ctx, "TRUNCATE webhook_deliveries"); err != nil {
			t.Fatalf("truncate: %v", err)
		}
	}

	count := func(t *testing.T) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	t.Run("enqueue round trips the pointer fields", func(t *testing.T) {
		reset(t)

		n, err := q.EnqueueDelivery(ctx, db.EnqueueDeliveryParams{
			DeliveryID: "delivery-abc-123",
			Repo:       "orkanoio/orkano",
			EventType:  "push",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if n != 1 {
			t.Fatalf("rows affected = %d, want 1", n)
		}

		if got := count(t); got != 1 {
			t.Fatalf("count = %d, want 1", got)
		}

		var (
			deliveryID, repo, eventType string
			receivedAt                  time.Time
		)
		row := pool.QueryRow(ctx,
			"SELECT delivery_id, repo, event_type, received_at FROM webhook_deliveries WHERE delivery_id = $1",
			"delivery-abc-123")
		if err := row.Scan(&deliveryID, &repo, &eventType, &receivedAt); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		if deliveryID != "delivery-abc-123" || repo != "orkanoio/orkano" || eventType != "push" {
			t.Fatalf("stored (%q, %q, %q), want (delivery-abc-123, orkanoio/orkano, push)", deliveryID, repo, eventType)
		}
		// received_at is stamped by the DB's DEFAULT now(). Assert it is fresh
		// without a tight before/after window: Postgres runs in a VM whose clock
		// can drift tens of ms from the host, so comparing a DB-stamped time
		// against host time.Now() flakes. A generous tolerance still proves the
		// default fired (not zero, NULL, or a fixed sentinel) while absorbing any
		// realistic host/container skew.
		if skew := time.Since(receivedAt); skew < -5*time.Minute || skew > 5*time.Minute {
			t.Fatalf("received_at %v is not within 5m of now — DEFAULT now() did not stamp a fresh time", receivedAt)
		}
	})

	t.Run("duplicate delivery id collapses to one row", func(t *testing.T) {
		reset(t)

		first, err := q.EnqueueDelivery(ctx, db.EnqueueDeliveryParams{
			DeliveryID: "dup-1", Repo: "orkanoio/first", EventType: "push",
		})
		if err != nil || first != 1 {
			t.Fatalf("first enqueue: n=%d err=%v", first, err)
		}

		// Same delivery id, different repo: ON CONFLICT DO NOTHING must keep the
		// original row and report zero rows affected.
		second, err := q.EnqueueDelivery(ctx, db.EnqueueDeliveryParams{
			DeliveryID: "dup-1", Repo: "orkanoio/second", EventType: "push",
		})
		if err != nil {
			t.Fatalf("second enqueue: %v", err)
		}
		if second != 0 {
			t.Fatalf("second enqueue rows affected = %d, want 0", second)
		}

		if got := count(t); got != 1 {
			t.Fatalf("count = %d, want 1", got)
		}

		var repo string
		if err := pool.QueryRow(ctx,
			"SELECT repo FROM webhook_deliveries WHERE delivery_id = $1", "dup-1").Scan(&repo); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if repo != "orkanoio/first" {
			t.Fatalf("repo = %q, want orkanoio/first (the conflicting write must not overwrite)", repo)
		}
	})

	t.Run("schema carries no payload column", func(t *testing.T) {
		rows, err := pool.Query(ctx, `
			SELECT column_name FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'webhook_deliveries'
			ORDER BY column_name`)
		if err != nil {
			t.Fatalf("query columns: %v", err)
		}
		cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("collect columns: %v", err)
		}
		// The webhook is a doorbell: only pointer fields exist, so no later code
		// can read a stored payload it was never meant to trust (INV-03/INV-04).
		want := []string{"delivery_id", "event_type", "id", "received_at", "repo"}
		if !slices.Equal(cols, want) {
			t.Fatalf("columns = %v, want %v", cols, want)
		}
	})

	t.Run("migrate is idempotent", func(t *testing.T) {
		if err := db.Migrate(ctx, dsn); err != nil {
			t.Fatalf("re-running migrate must be a no-op, got: %v", err)
		}
	})
}
