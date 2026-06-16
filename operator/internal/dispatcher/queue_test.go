package dispatcher

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/orkanoio/orkano/internal/db"
)

// Multi-arch index digest so the image resolves on CI amd64 and local arm64
// (kept in sync with internal/db's setup_test.go).
const postgresImage = "postgres:17-alpine@sha256:979c4379dd698aba0b890599a6104e082035f98ef31d9b9291ec22f2b13059ca"

// startPostgres boots one throwaway Postgres and returns its dsn (migrations not
// applied). Skipped when no container runtime is reachable, so `make test` stays
// green without Docker while CI runs it for real.
func startPostgres(t *testing.T) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	pg, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("orkano"),
		postgres.WithUsername("orkano"),
		postgres.WithPassword("orkano-test"),
		postgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, pg)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

func withCreds(t *testing.T, dsn, user, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// TestPgxQueue proves the production consume path end to end: PgxQueue over the
// least-privilege orkano_dispatcher role, the Ack/Nack transaction semantics,
// and that an outstanding claim holds its row lock so a concurrent claim skips
// it (FOR UPDATE SKIP LOCKED).
func TestPgxQueue(t *testing.T) {
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

	// init sets the dispatcher password via ALTER ROLE at install; the migration
	// ships only the privilege shape. Consume as that role, like production.
	if _, err := admin.Exec(ctx, "ALTER ROLE orkano_dispatcher WITH PASSWORD 'disp-pw'"); err != nil {
		t.Fatalf("set dispatcher password: %v", err)
	}
	pool, err := pgxpool.New(ctx, withCreds(t, dsn, "orkano_dispatcher", "disp-pw"))
	if err != nil {
		t.Fatalf("dispatcher pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := &PgxQueue{Pool: pool}

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
	count := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	t.Run("ack removes the row", func(t *testing.T) {
		reset(t)
		seed(t, "d1", "orkanoio/a")

		d, err := q.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if d == nil || d.DeliveryID != "d1" {
			t.Fatalf("claimed %+v, want d1", d)
		}
		if err := d.Ack(ctx); err != nil {
			t.Fatalf("ack: %v", err)
		}
		if got := count(t); got != 0 {
			t.Fatalf("rows after ack = %d, want 0", got)
		}
	})

	t.Run("nack leaves the row for a later claim", func(t *testing.T) {
		reset(t)
		seed(t, "d1", "orkanoio/a")

		d, err := q.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := d.Nack(ctx); err != nil {
			t.Fatalf("nack: %v", err)
		}
		if got := count(t); got != 1 {
			t.Fatalf("rows after nack = %d, want 1 (row stays)", got)
		}
		// The lock is released, so the next claim re-hands the same row.
		again, err := q.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("re-claim: %v", err)
		}
		if again == nil || again.DeliveryID != "d1" {
			t.Fatalf("re-claim = %+v, want d1 back", again)
		}
		if err := again.Ack(ctx); err != nil {
			t.Fatalf("ack: %v", err)
		}
	})

	t.Run("empty queue returns nil", func(t *testing.T) {
		reset(t)
		d, err := q.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("claim on empty: %v", err)
		}
		if d != nil {
			t.Fatalf("claimed %+v on empty queue, want nil", d)
		}
	})

	t.Run("an outstanding claim holds its lock; a concurrent claim skips it", func(t *testing.T) {
		reset(t)
		seed(t, "first", "orkanoio/a")
		seed(t, "second", "orkanoio/b")

		// first is claimed and NOT finalized: its row lock is held.
		first, err := q.ClaimNext(ctx)
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}
		if first.DeliveryID != "first" {
			t.Fatalf("first claim = %q, want first", first.DeliveryID)
		}

		// A concurrent claim must SKIP the locked row and get the next one. A
		// short deadline turns a dropped SKIP LOCKED (a plain FOR UPDATE would
		// block on the held lock) into a fast failure instead of a hang.
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		second, err := q.ClaimNext(claimCtx)
		if err != nil {
			t.Fatalf("second claim: %v (a deadline here means FOR UPDATE blocked — is SKIP LOCKED missing?)", err)
		}
		if second == nil || second.DeliveryID != "second" {
			t.Fatalf("second claim = %+v, want the unlocked row second", second)
		}

		if err := first.Ack(ctx); err != nil {
			t.Fatalf("ack first: %v", err)
		}
		if err := second.Ack(ctx); err != nil {
			t.Fatalf("ack second: %v", err)
		}
		if got := count(t); got != 0 {
			t.Fatalf("rows after both acks = %d, want 0", got)
		}
	})
}
