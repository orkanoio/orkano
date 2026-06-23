package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"

	// pgx's database/sql driver, registered as "pgx" — SetupRoles runs DDL over
	// a short-lived *sql.DB, mirroring Migrate.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// ReceiverRole and DispatcherRole are the least-privilege login roles migration
// 00002 creates without passwords (it version-controls only the privilege
// shape). SetupRoles assigns their passwords at install time.
const (
	ReceiverRole   = "orkano_receiver"
	DispatcherRole = "orkano_dispatcher"
)

// safePassword bounds role passwords to characters with no SQL-string meaning,
// so the ALTER ROLE statements below can carry the value inline without risk of
// injection or breakage. Callers generate high-entropy URL-safe passwords (hex
// or base64url — no quotes, no backslashes), which this set covers.
var safePassword = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// SetupRoles assigns login passwords to the receiver and dispatcher roles
// created by migration 00002. It is the install-time step the platform's
// migration Job runs after Migrate, connecting with the superuser DSN; the
// migration deliberately ships those roles passwordless so the credential lives
// only in the install-generated Secrets, never in version control. Idempotent:
// ALTER ROLE overwrites any existing password.
//
// PostgreSQL cannot parameterize DDL, so the passwords are placed inline —
// guarded by safePassword so a value carrying a quote or backslash is rejected
// rather than concatenated. The role names are fixed constants, never input.
func SetupRoles(ctx context.Context, superuserDSN, receiverPassword, dispatcherPassword string) error {
	roles := []struct{ name, password string }{
		{ReceiverRole, receiverPassword},
		{DispatcherRole, dispatcherPassword},
	}
	for _, r := range roles {
		if r.password == "" {
			return fmt.Errorf("password for %s is empty", r.name)
		}
		if !safePassword.MatchString(r.password) {
			return fmt.Errorf("password for %s contains unsupported characters", r.name)
		}
	}

	sqlDB, err := sql.Open("pgx", superuserDSN)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	for _, r := range roles {
		if _, err := sqlDB.ExecContext(ctx, "ALTER ROLE "+r.name+" LOGIN PASSWORD '"+r.password+"'"); err != nil {
			return fmt.Errorf("set password for %s: %w", r.name, err)
		}
	}
	return nil
}
