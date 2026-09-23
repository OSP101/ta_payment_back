package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// grad_lump_ledger.go keeps an exported month's graduate-special lump fixed.
//
// gradLumpByMonth splits a flat per-term lump over the term's months by
// weight. Recomputed live, a later month gaining weight shrank a month that
// finance had already been sent (4,000 → 1,000 in the audit's reproduction),
// and the next claim document printed the smaller figure for the same month.
// Exporting a course now records, per holder and exported month, the amount
// the document carried (migration 0119); the split only redistributes what is
// left of the lump over the months that are not frozen yet.

// frozenGradLump is what has already been paid out of one (course, TA) lump.
type frozenGradLump struct {
	months map[string]float64
	total  float64
	// basis is the whole-term lump the first freeze used. Once anything is
	// frozen the lump is this figure for the rest of the term — a rate change
	// mid-term applies only to lumps nothing has been paid from yet, so the
	// frozen months can never add up to more than the lump they are part of.
	basis float64
}

func (f *frozenGradLump) any() bool { return f != nil && len(f.months) > 0 }

// ledgerQuerier is satisfied by both the pool and a transaction.
type ledgerQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func loadFrozenGradLump(ctx context.Context, q ledgerQuerier, courseID, taID uuid.UUID) (*frozenGradLump, error) {
	rows, err := q.Query(ctx, `
		SELECT year_month, baht::float8, lump_basis::float8
		FROM grad_lump_ledger
		WHERE teaching_course_id = $1 AND ta_id = $2`, courseID, taID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &frozenGradLump{months: map[string]float64{}}
	for rows.Next() {
		var ym string
		var baht, basis float64
		if err := rows.Scan(&ym, &baht, &basis); err != nil {
			return nil, err
		}
		out.months[ym] = baht
		out.total += baht
		out.basis = basis
	}
	return out, rows.Err()
}

// gradSpecialHolderIDs lists the TAs whose lump a course's claim documents
// carry — the same set buildExportRows pays (approved request, master/phd on a
// special-track section).
func gradSpecialHolderIDs(ctx context.Context, q ledgerQuerier, courseID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT a.ta_id
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec ON sec.id = a.section_id
		WHERE r.teaching_course_id = $1
		  AND a.level::text IN ('master','phd') AND sec.track = 'special'`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FreezeGradLumps records, for every grad-special holder of the course, the
// lump share of each exported month — the figure the claim documents just
// built for those months carry. Called when a course's claim ZIP is downloaded,
// right after the months are locked; must not be best-effort, or the next
// document could print a different figure for a month finance already has.
//
// Idempotent: a month already frozen keeps its first figure (ON CONFLICT DO
// NOTHING), which is exactly what a re-export of the same month must print.
// An empty month list means the whole term, as for the export itself.
func (s *ExportService) FreezeGradLumps(ctx context.Context, actor, courseID uuid.UUID, months []string) error {
	if len(months) == 0 {
		all, err := s.CourseTermMonths(ctx, courseID)
		if err != nil {
			return err
		}
		for _, m := range all {
			months = append(months, m.YearMonth)
		}
	}
	if len(months) == 0 {
		return nil
	}
	holders, err := gradSpecialHolderIDs(ctx, s.pool, courseID)
	if err != nil || len(holders) == 0 {
		return err
	}
	var pr struct{ lumpsum, termCap float64 }
	if err := s.pool.QueryRow(ctx,
		`SELECT graduate_special_lumpsum, grad_special_term_cap FROM `+payRatesInForce).
		Scan(&pr.lumpsum, &pr.termCap); err != nil {
		return err
	}
	lump := pr.lumpsum
	if pr.termCap > 0 && lump > pr.termCap {
		lump = pr.termCap
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Two exports of the same course at once must not each freeze a split
	// computed without the other's rows.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 7))`,
		"grad_lump_ledger/"+courseID.String()); err != nil {
		return err
	}
	for _, ta := range holders {
		byMonth, basis, err := s.gradLumpSplit(ctx, tx, courseID, ta, lump, true)
		if err != nil {
			return err
		}
		for _, ym := range months {
			if _, err := tx.Exec(ctx, `
				INSERT INTO grad_lump_ledger (teaching_course_id, ta_id, year_month, baht, lump_basis, frozen_by)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (teaching_course_id, ta_id, year_month) DO NOTHING`,
				courseID, ta, ym, round2(byMonth[ym]), round2(basis), actor); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// withoutFrozen drops the frozen months from a weight map.
func withoutFrozen(weights map[string]float64, frozen *frozenGradLump) map[string]float64 {
	if !frozen.any() {
		return weights
	}
	out := map[string]float64{}
	for ym, w := range weights {
		if _, done := frozen.months[ym]; !done {
			out[ym] = w
		}
	}
	return out
}
