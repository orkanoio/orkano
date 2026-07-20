package dispatcher

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orkanoio/orkano/internal/db"
)

// Queue hands out webhook doorbells one at a time and lets the dispatcher
// finalize each: Ack removes the row (processed, or permanently unprocessable),
// Nack leaves it for a later poll (a transient failure). The implementation
// holds a row lock from claim until finalize, so the dispatcher can act on a
// delivery and only then commit its removal — at-least-once delivery, made
// idempotent by the Build's deterministic push or per-request name.
type Queue interface {
	// ClaimNext claims the oldest unprocessed delivery and locks its row until
	// the returned Delivery is Ack'd or Nack'd. It returns (nil, nil) when the
	// queue is empty.
	ClaimNext(ctx context.Context) (*Delivery, error)
}

// Delivery is one claimed doorbell plus the handles to finalize it. Exactly one
// of Ack or Nack must be called, and only once.
type Delivery struct {
	ID         int64
	DeliveryID string
	Repo       string
	// EventType is "push" for receiver doorbells and "manual" for an authenticated
	// dashboard request.
	EventType string
	// AppName narrows a manual request to exactly one App. Empty push doorbells
	// retain repo-wide monorepo fan-out.
	AppName string

	ack  func(context.Context) error
	nack func(context.Context) error
}

// Ack removes the delivery from the queue (it was processed or is permanently
// unprocessable) and releases its lock.
func (d *Delivery) Ack(ctx context.Context) error { return d.ack(ctx) }

// Nack leaves the delivery in the queue for a later poll (a transient failure)
// and releases its lock.
func (d *Delivery) Nack(ctx context.Context) error { return d.nack(ctx) }

// PgxQueue is the production Queue, backed by the platform Postgres and
// consuming under the least-privilege orkano_dispatcher role.
type PgxQueue struct {
	Pool *pgxpool.Pool
}

// ClaimNext begins a transaction, claims the oldest delivery with
// FOR UPDATE SKIP LOCKED, and returns it with Ack/Nack bound to that
// transaction: Ack deletes the row and commits, Nack rolls back (leaving the
// row and releasing the lock). The transaction — and the pooled connection it
// holds — stays open until the caller finalizes, so the dispatcher must always
// Ack or Nack a claimed delivery.
func (q *PgxQueue) ClaimNext(ctx context.Context) (*Delivery, error) {
	tx, err := q.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin claim transaction: %w", err)
	}
	// The transaction stays open across the dispatcher's GitHub round-trips (it
	// holds the row lock until Ack/Nack), which can outlast a hardened or managed
	// Postgres's idle_in_transaction_session_timeout and get the transaction
	// killed server-side. Disable it for this transaction only — SET LOCAL
	// reverts on commit/rollback, so it never touches other pooled connections or
	// any server-wide setting. The parameter is USERSET, so the dispatcher role
	// may set it.
	if _, err := tx.Exec(ctx, "SET LOCAL idle_in_transaction_session_timeout = 0"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("relaxing idle-in-transaction timeout for claim: %w", err)
	}
	row, err := db.New(tx).ClaimDelivery(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		// Empty queue: end the transaction and report nothing claimed.
		_ = tx.Rollback(ctx)
		return nil, nil
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("claim delivery: %w", err)
	}

	id := row.ID
	return &Delivery{
		ID:         row.ID,
		DeliveryID: row.DeliveryID,
		Repo:       row.Repo,
		EventType:  row.EventType,
		AppName:    row.AppName.String,
		ack: func(ctx context.Context) error {
			if err := db.New(tx).DeleteDelivery(ctx, id); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("delete delivery %d: %w", id, err)
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit delivery %d removal: %w", id, err)
			}
			return nil
		},
		nack: func(ctx context.Context) error {
			if err := tx.Rollback(ctx); err != nil {
				return fmt.Errorf("rollback delivery %d: %w", id, err)
			}
			return nil
		},
	}, nil
}
