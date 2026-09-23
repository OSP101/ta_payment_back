package service

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// writeAudited runs a change and the audit row recording it inside ONE
// transaction, so the two land together or neither does.
//
// The alternative — write on the pool, then audit on the pool — has a window
// between them. If the audit fails there, the change has already happened and
// there is no honest answer left: report success and the trail has a hole
// nobody can see, or report failure for something that did in fact occur. Both
// leave the record disagreeing with the money.
//
// `write` receives the transaction and must do all of its work on it; anything
// it runs on the pool instead is outside the guarantee.
func writeAudited(
	ctx context.Context,
	pool *pgxpool.Pool,
	aud *audit.Auditor,
	e audit.Entry,
	write func(tx pgx.Tx) error,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	// Safe after a successful Commit: pgx makes Rollback a no-op once the
	// transaction is closed, so the deferred call only matters on the paths
	// that return early.
	defer tx.Rollback(ctx)

	if err := write(tx); err != nil {
		return err
	}
	if err := aud.LogTx(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// writeAuditedLocked is writeAudited for a change whose BEFORE-image must be
// read inside the same transaction, after the caller has taken its lock.
//
// writeAudited receives the entry by value before the transaction opens, so any
// before-image it carries was read outside the transaction — a concurrent
// writer could commit between that read and this write, and the trail would
// record a "before" that was already gone. Here `write` gets the entry by
// pointer and fills in Before itself, on the transaction.
func writeAuditedLocked(
	ctx context.Context,
	pool *pgxpool.Pool,
	aud *audit.Auditor,
	e audit.Entry,
	write func(tx pgx.Tx, e *audit.Entry) error,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := write(tx, &e); err != nil {
		return err
	}
	if err := aud.LogTx(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
