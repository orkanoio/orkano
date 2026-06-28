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

// ReceiverRole, DispatcherRole, and DashboardRole are the least-privilege login
// roles migrations 00002/00004 create without passwords (they version-control
// only the privilege shape). SetupRoles assigns their passwords at install time.
const (
	ReceiverRole   = "orkano_receiver"
	DispatcherRole = "orkano_dispatcher"
	DashboardRole  = "orkano_dashboard"
)

// RolePasswords carries the login password for each least-privilege role
// SetupRoles assigns. Named fields keep the three same-typed values from being
// transposed at a call site.
type RolePasswords struct {
	Receiver   string
	Dispatcher string
	Dashboard  string
}

// safePassword bounds role passwords to characters with no SQL-string meaning,
// so the ALTER ROLE statements below can carry the value inline without risk of
// injection or breakage. Callers generate high-entropy URL-safe passwords (hex
// or base64url — no quotes, no backslashes), which this set covers.
var safePassword = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// SetupRoles assigns login passwords to the least-privilege roles created by
// migrations 00002/00004. It is the install-time step the platform's migration
// Job runs after Migrate, connecting with the superuser DSN; the migrations
// deliberately ship those roles passwordless so the credentials live only in the
// install-generated Secrets, never in version control. Idempotent: ALTER ROLE
// overwrites any existing password.
//
// PostgreSQL cannot parameterize DDL, so the passwords are placed inline —
// guarded by safePassword so a value carrying a quote or backslash is rejected
// rather than concatenated. The role names are fixed constants, never input.
func SetupRoles(ctx context.Context, superuserDSN string, pw RolePasswords) error {
	roles := []struct{ name, password string }{
		{ReceiverRole, pw.Receiver},
		{DispatcherRole, pw.Dispatcher},
		{DashboardRole, pw.Dashboard},
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
