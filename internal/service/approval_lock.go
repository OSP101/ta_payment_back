package service

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// lockApprovalTAs protects rules spanning courses (overlap and daily pay), not
// just a single course's budget. Acquire every TA in order BEFORE course locks
// and before reading approval snapshots. Migration 0157 uses the same namespace.
func lockApprovalTAs(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) error {
	ordered := append([]uuid.UUID(nil), ids...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].String() < ordered[j].String() })
	for i, id := range ordered {
		if i > 0 && id == ordered[i-1] {
			continue
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,157))`, id); err != nil {
			return err
		}
	}
	return nil
}
