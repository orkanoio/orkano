package server

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orkanoio/orkano/internal/db"
)

// Store is exactly the slice of the generated query surface the auth handlers
// call, plus the one composite CreateAdmin the transactional redeem needs.
// *db.Queries satisfies every method except CreateAdmin; the pgStore adapter
// supplies that. Defining it as an interface keeps the handlers unit-testable
// against an in-memory fake while the transaction path is integration-tested.
type Store interface {
	CountConfirmedAdmins(ctx context.Context) (int64, error)
	GetUserByUsername(ctx context.Context, username string) (db.GetUserByUsernameRow, error)
	GetUserByID(ctx context.Context, id int64) (db.GetUserByIDRow, error)
	// The OIDC sign-in pair (ADR-0016): look up an IdP-linked identity by its
	// durable (issuer, subject) key, or just-in-time provision a credential-less
	// anchor for a first login. *db.Queries supplies both.
	GetUserByOIDC(ctx context.Context, arg db.GetUserByOIDCParams) (db.GetUserByOIDCRow, error)
	CreateOIDCUser(ctx context.Context, arg db.CreateOIDCUserParams) (db.CreateOIDCUserRow, error)
	ConfirmUserTOTP(ctx context.Context, id int64) error
	IncrementFailedLogins(ctx context.Context, id int64) (int32, error)
	LockUser(ctx context.Context, arg db.LockUserParams) error
	ResetFailedLogins(ctx context.Context, id int64) error
	CreateSession(ctx context.Context, arg db.CreateSessionParams) error
	GetSession(ctx context.Context, tokenHash string) (db.Session, error)
	TouchSession(ctx context.Context, tokenHash string) error
	DeleteSession(ctx context.Context, tokenHash string) error
	MarkSessionReauth(ctx context.Context, tokenHash string) error
	ConsumeRecoveryCode(ctx context.Context, arg db.ConsumeRecoveryCodeParams) (int64, error)
	AppendAuditEntry(ctx context.Context, arg db.AppendAuditEntryParams) error
	EnqueueManualDelivery(ctx context.Context, arg db.EnqueueManualDeliveryParams) (int64, error)

	// The M2.4 read/write views: the deploy timeline an App detail page shows
	// (Build CRs get GC'd, so this is the durable record) and the audit log
	// (INV-08). *db.Queries supplies all three; the dashboard never UPDATE/DELETEs
	// audit_log (the role forbids it).
	RecordDeploy(ctx context.Context, arg db.RecordDeployParams) (db.DeployHistory, error)
	ListAppDeploys(ctx context.Context, arg db.ListAppDeploysParams) ([]db.DeployHistory, error)
	ListAuditEntries(ctx context.Context, arg db.ListAuditEntriesParams) ([]db.AuditLog, error)

	// The onboarding wizard's setup-state rows (M2.6, migration 00007): non-secret
	// pointers and choices only (INV-03) — the access-mode choice and the GitHub/
	// OIDC connect markers the dashboard cannot derive from the value-blind
	// Secrets it writes. *db.Queries supplies both (its GetSetting point-read
	// stays off this interface until a handler needs it — small surface).
	UpsertSetting(ctx context.Context, arg db.UpsertSettingParams) error
	ListSettings(ctx context.Context) ([]db.Setting, error)

	// CreateAdmin atomically clears any abandoned enrollment, creates the single
	// admin (TOTP unconfirmed), and stores its recovery-code hashes — all in one
	// transaction so a redeem can never leave a half-built account.
	CreateAdmin(ctx context.Context, arg CreateAdminParams) (db.CreateUserRow, error)
}

// CreateAdminParams carries everything the redeem transaction inserts. The TOTP
// seed is already sealed (encrypted at rest) and the recovery codes are already
// hashed by the caller — this layer stores opaque material only.
type CreateAdminParams struct {
	Username           string
	PasswordHash       string
	SealedTOTPSecret   string
	RecoveryCodeHashes []string
}

// pgStore is the production Store. It embeds *db.Queries so every simple query
// passes through unchanged; only CreateAdmin is hand-written to run in a pgx
// transaction via Queries.WithTx.
type pgStore struct {
	pool *pgxpool.Pool
	*db.Queries
}

// NewStore builds the production Store over a pgx pool.
func NewStore(pool *pgxpool.Pool) Store {
	return &pgStore{pool: pool, Queries: db.New(pool)}
}

func (s *pgStore) CreateAdmin(ctx context.Context, arg CreateAdminParams) (db.CreateUserRow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.CreateUserRow{}, fmt.Errorf("begin redeem transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.WithTx(tx)

	if err := q.DeleteUnconfirmedUsers(ctx); err != nil {
		return db.CreateUserRow{}, fmt.Errorf("clear abandoned enrollment: %w", err)
	}

	user, err := q.CreateUser(ctx, db.CreateUserParams{
		Username:        arg.Username,
		PasswordHash:    arg.PasswordHash,
		TotpSecret:      arg.SealedTOTPSecret,
		TotpConfirmedAt: pgtype.Timestamptz{}, // NULL: second factor not yet confirmed
	})
	if err != nil {
		return db.CreateUserRow{}, fmt.Errorf("create admin user: %w", err)
	}

	for _, hash := range arg.RecoveryCodeHashes {
		if err := q.InsertRecoveryCode(ctx, db.InsertRecoveryCodeParams{
			UserID:   user.ID,
			CodeHash: hash,
		}); err != nil {
			return db.CreateUserRow{}, fmt.Errorf("store recovery code: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return db.CreateUserRow{}, fmt.Errorf("commit redeem transaction: %w", err)
	}
	return user, nil
}

// pgStore is a complete Store at compile time.
var _ Store = (*pgStore)(nil)
