package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

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

// GradLumpSnapshot is the graduate-special lump split ONE claim pack printed.
//
// A course ZIP carries the lump in several documents (the export rows, the
// combined book, the graduate evidence book, the budget settlement), and each
// used to compute the split on its own; the freeze then computed it once more.
// Anything that moved the weights in between — an approval, a course-date
// edit, a second export of the same course — left the ledger holding a figure
// the pack never printed. Computed once at the start of BuildCourseZip and
// carried on the context, the snapshot is what every document reads, and it is
// exactly what FreezeGradLumpSnapshot writes to the ledger.
type GradLumpSnapshot struct {
	CourseID uuid.UUID
	// Lump is the whole-term lump offered to holders nothing is frozen for yet
	// (LEAST(lumpsum, term cap) of the rate in force).
	Lump float64

	mu       sync.Mutex
	lumpRead bool
	byTA  map[uuid.UUID]map[string]float64
	basis map[uuid.UUID]float64
}

func copyMonths(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// gradLumpInForce is the per-term lump every document computes from the rate
// in force: the lump sum, capped by the per-term ceiling when one is set.
func gradLumpInForce(ctx context.Context, q ledgerQuerier) (float64, error) {
	var lumpsum, termCap float64
	if err := q.QueryRow(ctx,
		`SELECT graduate_special_lumpsum, grad_special_term_cap FROM `+payRatesInForce).
		Scan(&lumpsum, &termCap); err != nil {
		return 0, err
	}
	if termCap > 0 && lumpsum > termCap {
		return termCap, nil
	}
	return lumpsum, nil
}

// computeGradLumpSnapshot splits every current holder's lump once.
func (s *ExportService) computeGradLumpSnapshot(ctx context.Context, courseID uuid.UUID) (*GradLumpSnapshot, error) {
	snap := &GradLumpSnapshot{
		CourseID: courseID,
		byTA:     map[uuid.UUID]map[string]float64{},
		basis:    map[uuid.UUID]float64{},
	}
	holders, err := gradSpecialHolderIDs(ctx, s.pool, courseID)
	if err != nil {
		return nil, err
	}
	if len(holders) == 0 {
		// Nobody to split for. The lump is still read only when needed by a
		// late holder (splitFor), so a course with no pay rate yet — none of
		// whose documents carries a lump — is not refused here.
		return snap, nil
	}
	if snap.Lump, err = gradLumpInForce(ctx, s.pool); err != nil {
		return nil, err
	}
	snap.lumpRead = true
	for _, ta := range holders {
		byMonth, basis, err := s.gradLumpSplit(ctx, s.pool, courseID, ta, snap.Lump, true)
		if err != nil {
			return nil, err
		}
		snap.byTA[ta] = byMonth
		snap.basis[ta] = basis
	}
	return snap, nil
}

// splitFor returns the snapshot's split for one holder, computing and
// RECORDING it when the holder was not in the snapshot yet (a request approved
// while the pack was being built) — so whatever a document printed is also
// what the freeze writes, never a figure the freeze does not know about.
func (snap *GradLumpSnapshot) splitFor(ctx context.Context, s *ExportService, taID uuid.UUID) (map[string]float64, error) {
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if m, ok := snap.byTA[taID]; ok {
		return copyMonths(m), nil
	}
	if !snap.lumpRead {
		lump, err := gradLumpInForce(ctx, s.pool)
		if err != nil {
			return nil, err
		}
		snap.Lump, snap.lumpRead = lump, true
	}
	byMonth, basis, err := s.gradLumpSplit(ctx, s.pool, snap.CourseID, taID, snap.Lump, true)
	if err != nil {
		return nil, err
	}
	snap.byTA[taID] = byMonth
	snap.basis[taID] = basis
	return copyMonths(byMonth), nil
}

type gradLumpSnapshotKey struct{}

// WithGradLumpSnapshot makes every gradLumpByMonth call for snap's course (with
// approvedOnly, as the documents use) read the snapshot instead of recomputing.
func WithGradLumpSnapshot(ctx context.Context, snap *GradLumpSnapshot) context.Context {
	if snap == nil {
		return ctx
	}
	return context.WithValue(ctx, gradLumpSnapshotKey{}, snap)
}

func gradLumpSnapshotFrom(ctx context.Context, courseID uuid.UUID) *GradLumpSnapshot {
	snap, _ := ctx.Value(gradLumpSnapshotKey{}).(*GradLumpSnapshot)
	if snap == nil || snap.CourseID != courseID {
		return nil
	}
	return snap
}

// FreezeGradLumps freezes a freshly computed split. Kept for callers that do
// not build a pack first; the export itself freezes the snapshot the pack
// printed (FreezeGradLumpSnapshot).
func (s *ExportService) FreezeGradLumps(ctx context.Context, actor, courseID uuid.UUID, months []string) error {
	snap, err := s.computeGradLumpSnapshot(ctx, courseID)
	if err != nil {
		return err
	}
	return s.FreezeGradLumpSnapshot(ctx, actor, snap, months)
}

// FreezeGradLumpSnapshot records, for every holder in snap, the lump share of
// each exported month — exactly the figure the claim pack printed. Called when
// a course's claim ZIP is downloaded, right after the months are locked; must
// not be best-effort, or the next document could print a different figure for
// a month finance already has.
//
// Idempotent: a month already frozen keeps its first figure (ON CONFLICT DO
// NOTHING). The rows are then read back and compared with the snapshot: a
// mismatch means another export froze different figures first, and this pack
// must not leave the server — the caller fails the request, and a re-download
// prints the frozen figures. An empty month list means the whole term.
func (s *ExportService) FreezeGradLumpSnapshot(ctx context.Context, actor uuid.UUID, snap *GradLumpSnapshot, months []string) error {
	if snap == nil {
		return nil
	}
	courseID := snap.CourseID
	if len(months) == 0 {
		all, err := s.CourseTermMonths(ctx, courseID)
		if err != nil {
			return err
		}
		for _, m := range all {
			months = append(months, m.YearMonth)
		}
	}
	snap.mu.Lock()
	byTA := make(map[uuid.UUID]map[string]float64, len(snap.byTA))
	basis := make(map[uuid.UUID]float64, len(snap.basis))
	for ta, m := range snap.byTA {
		byTA[ta] = copyMonths(m)
		basis[ta] = snap.basis[ta]
	}
	snap.mu.Unlock()
	if len(months) == 0 || len(byTA) == 0 {
		return nil
	}
	holders := make([]uuid.UUID, 0, len(byTA))
	for ta := range byTA {
		holders = append(holders, ta)
	}
	sort.Slice(holders, func(i, j int) bool { return holders[i].String() < holders[j].String() })

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Two exports of the same course at once must not interleave their rows.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 7))`,
		"grad_lump_ledger/"+courseID.String()); err != nil {
		return err
	}
	for _, ta := range holders {
		for _, ym := range months {
			if _, err := tx.Exec(ctx, `
				INSERT INTO grad_lump_ledger (teaching_course_id, ta_id, year_month, baht, lump_basis, frozen_by)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (teaching_course_id, ta_id, year_month) DO NOTHING`,
				courseID, ta, ym, round2(byTA[ta][ym]), round2(basis[ta]), actor); err != nil {
				return err
			}
		}
	}

	// Read back what the ledger now holds for these cells and compare with
	// what the pack printed.
	rows, err := tx.Query(ctx, `
		SELECT ta_id, year_month, baht::float8
		FROM grad_lump_ledger
		WHERE teaching_course_id = $1 AND ta_id = ANY($2) AND year_month = ANY($3)`,
		courseID, holders, months)
	if err != nil {
		return err
	}
	var drift []string
	for rows.Next() {
		var ta uuid.UUID
		var ym string
		var baht float64
		if err := rows.Scan(&ta, &ym, &baht); err != nil {
			rows.Close()
			return err
		}
		if math.Abs(baht-round2(byTA[ta][ym])) > 0.005 {
			drift = append(drift, ym)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(drift) > 0 {
		sort.Strings(drift)
		return Conflict(fmt.Sprintf(
			"ยอดเหมาจ่ายบัณฑิตภาคพิเศษของเดือน %s ถูกกำหนดไว้แล้วจากการส่งออกอีกครั้งที่เกิดพร้อมกัน "+
				"เอกสารชุดนี้จึงไม่ถูกส่งออก — กรุณากดดาวน์โหลดใหม่", strings.Join(uniqueStrings(drift), ", ")))
	}
	return tx.Commit(ctx)
}

func uniqueStrings(in []string) []string {
	out := in[:0:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
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
