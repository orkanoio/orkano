// Package db owns the platform Postgres schema, its goose migrations, and the
// sqlc-generated queries shared by the receiver and the operator's dispatcher.
// It holds pointers (webhook deliveries, deploy history, audit), never secret
// values and never webhook payloads (INV-03).
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"

	// pgx's database/sql driver, registered as "pgx" — used only to apply
	// migrations; runtime queries use the pgx-native pool via New.
	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies every pending migration to the database at dsn. It is
// idempotent: an already-current database is a no-op. The connection it opens is
// short-lived and closed before returning.
func Migrate(ctx context.Context, dsn string) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("sub migrations fs: %w", err)
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, sub)
	if err != nil {
		return fmt.Errorf("new goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
