package webhook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/orkanoio/orkano/internal/db"
	"github.com/orkanoio/orkano/receiver/internal/webhook"
)

// Mirrors internal/db/setup_test.go: a multi-arch index digest so the image
// resolves on CI amd64 and local arm64.
const postgresImage = "postgres:17-alpine@sha256:979c4379dd698aba0b890599a6104e082035f98ef31d9b9291ec22f2b13059ca"

const receiverPassword = "recv-pw"

// startReceiverDB boots Postgres, applies the migrations (creating the
// INSERT-only orkano_receiver role), and returns an admin pool plus a DSN
// authenticated as orkano_receiver — exactly the credential the deployed
// receiver holds. Skipped cleanly when no container runtime is reachable.
func startReceiverDB(t *testing.T) (*pgxpool.Pool, string) {
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

	adminDSN, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if err := db.Migrate(ctx, adminDSN); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	// init sets the role password at install via ALTER ROLE; play that step.
	if _, err := admin.Exec(ctx, "ALTER ROLE orkano_receiver WITH PASSWORD '"+receiverPassword+"'"); err != nil {
		t.Fatalf("set receiver password: %v", err)
	}

	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword("orkano_receiver", receiverPassword)
	return admin, u.String()
}

// TestReceiverEnqueuesUnderLeastPrivilege drives a real signed request through
// the handler backed by the generated query running as the INSERT-only role, and
// proves the doorbell row lands and duplicate deliveries collapse.
func TestReceiverEnqueuesUnderLeastPrivilege(t *testing.T) {
	admin, receiverDSN := startReceiverDB(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, receiverDSN)
	if err != nil {
		t.Fatalf("receiver pool: %v", err)
	}
	t.Cleanup(pool.Close)

	h := webhook.NewHandler(webhook.Config{
		Secret:    testSecret,
		Allowlist: []string{validRepo},
		Enqueuer:  db.New(pool),
	})

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/webhook", strings.NewReader(validBody))
		req.Header.Set(headerEvent, "push")
		req.Header.Set(headerDelivery, "delivery-abc")
		req.Header.Set(headerSignature, sign(testSecret, []byte(validBody)))
		rec := httptest.NewRecorder()
		h.Webhook(rec, req)
		return rec
	}

	if rec := post(); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want %d (body %q)", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var gotDelivery, gotRepo, gotEvent string
	if err := admin.QueryRow(ctx,
		"SELECT delivery_id, repo, event_type FROM webhook_deliveries").
		Scan(&gotDelivery, &gotRepo, &gotEvent); err != nil {
		t.Fatalf("read row back: %v", err)
	}
	if gotDelivery != "delivery-abc" || gotRepo != validRepo || gotEvent != "push" {
		t.Errorf("row = (%q,%q,%q), want (delivery-abc,%q,push)", gotDelivery, gotRepo, gotEvent, validRepo)
	}

	// A duplicate delivery is still accepted but must not add a second row.
	if rec := post(); rec.Code != http.StatusAccepted {
		t.Fatalf("duplicate delivery status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	var count int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries").Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Errorf("row count = %d, want 1 (duplicate must collapse)", count)
	}
}
