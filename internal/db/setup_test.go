package db_test

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Multi-arch index digest so the image resolves on CI amd64 and local arm64.
const postgresImage = "postgres:17-alpine@sha256:979c4379dd698aba0b890599a6104e082035f98ef31d9b9291ec22f2b13059ca"

// startPostgres boots one throwaway Postgres container and returns its dsn
// (migrations not yet applied). The test is skipped when no container runtime is
// reachable, so `make test` stays green on a machine without Docker while CI runs
// it for real.
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
