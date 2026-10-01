package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// export_month_ledger.go keeps an exported month's hourly money fixed
// (migration 0140).
//
// Exporting a month locks its work_logs (0124), so its HOURS cannot move. Its
// MONEY still could: every reader re-priced it from the rate in force and from
// a budget settlement that shares a short pool across the whole term, so a
// later month's approvals or a rate edit changed what an exported month was
// worth — and the export then refused even a plain re-download with 409
// because the "same" document no longer added up to what finance held
// (CP363205, 21,330.34 → 21,591).
//
// The export now records, per (TA, month, track), what the document said:
// earned (cost before the budget cut) and paid (what the settlement funded).
// Two hooks make every reader use it:
//
//   - claimCostByTASlot scales a locked month's คาบ so they sum to the frozen
//     earned figure (applyFrozenCosts) — the pay column, the transfer cover,
//     the course summary and the dashboards all price hours through it;
//   - the settlement takes the frozen paid figure off the top of the pool and
//     shares only what is left over the months that are still open
//     (settleTrackFrozen), so a locked month's funded share cannot move either.
//
// A month that is not exported/finance_sent right now is priced live; a month
// sent back and exported again carries the newer row (the corrected version).

// frozenKey is one locked cell of one budget pool.
type frozenKey struct {
	ta    uuid.UUID
	ym    string // Gregorian "2026-08"
	track string // "regular" | "special"
}

type frozenFigure struct {
	earned, paid float64
}

// loadFrozenMonths returns the ledger figures in force for a course: the
// newest row per cell, only for cells that are exported or finance_sent right
// now, and only rows written by the export that locked them (frozen_at no
// earlier than the cell's exported_at — a row from an earlier version whose
// month was since sent back and re-locked by an export that wrote no ledger
// must not resurface).
func loadFrozenMonths(ctx context.Context, q ledgerQuerier, courseID uuid.UUID) (map[frozenKey]frozenFigure, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (l.ta_id, l.year_month, l.track)
		       l.ta_id, l.year_month, l.track, l.earned_baht::float8, l.paid_baht::float8
		FROM export_month_ledger l
		JOIN teaching_courses tc   ON tc.id = l.teaching_course_id
		JOIN academic_terms trm    ON trm.id = tc.term_id
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		 AND sp.year_month = trm.academic_year::text || '-' || substr(l.year_month, 6, 2)
		JOIN submission_period_status st
		  ON st.submission_period_id = sp.id
		 AND st.ta_id = l.ta_id
		 AND st.teaching_course_id = tc.id
		WHERE l.teaching_course_id = $1
		  AND st.status IN ('exported','finance_sent')
		  AND st.exported_at IS NOT NULL
		  AND l.frozen_at >= st.exported_at
		ORDER BY l.ta_id, l.year_month, l.track, l.frozen_at DESC`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[frozenKey]frozenFigure{}
	for rows.Next() {
		var k frozenKey
		var f frozenFigure
		if err := rows.Scan(&k.ta, &k.ym, &k.track, &f.earned, &f.paid); err != nil {
			return nil, err
		}
		out[k] = f
	}
	return out, rows.Err()
}

// applyFrozenCosts rescales the คาบ of every locked cell so the cell sums to
// its frozen earned figure. Proportional, so the คาบ keep their relative
// sizes for the settlement and the printed rows; a rate change after export
// therefore leaves a locked month's money exactly where the document put it.
//
// A cell whose live cost is zero is left alone: its hours are locked, so the
// only way to get there is a rate of 0, and there is no คาบ to carry a figure.
func applyFrozenCosts(costs []taSlotCost, frozen map[frozenKey]frozenFigure) []taSlotCost {
	if len(frozen) == 0 {
		return costs
	}
	live := map[frozenKey]float64{}
	for _, c := range costs {
		live[frozenKey{c.TA, c.YearMonth, c.Track}] += c.Baht
	}
	for i, c := range costs {
		k := frozenKey{c.TA, c.YearMonth, c.Track}
		f, ok := frozen[k]
		if !ok {
			continue
		}
		if l := live[k]; l > 0 {
			costs[i].Baht = c.Baht * f.earned / l
		}
	}
	return costs
}

// frozenCostsFor is claimCostByTASlot's hook: the course's ledger applied to
// its live costs. A failed ledger read fails the pricing — silently pricing a
// locked month live is the bug this file exists to remove.
func (s *ExportService) frozenCostsFor(ctx context.Context, courseID uuid.UUID, costs []taSlotCost) ([]taSlotCost, error) {
	frozen, err := loadFrozenMonths(ctx, s.pool, courseID)
	if err != nil {
		return nil, err
	}
	return applyFrozenCosts(costs, frozen), nil
}

// settleTrackFrozen is settleTrack with the locked cells taken out of the
// sharing: their frozen paid figure is spent off the top of the pool, the
// rest of the pool is shared over the open months exactly as before, and the
// locked คาบ go back into the ledger carrying their frozen share. Without
// locked cells it IS settleTrack.
func settleTrackFrozen(mode SettlementMode, track string, cap, committed float64,
	slots []SlotSettlement, frozen map[frozenKey]frozenFigure) TrackSettlement {
	if len(frozen) == 0 {
		return settleTrack(mode, track, cap, committed, slots)
	}
	var live, locked []SlotSettlement
	lockedBaht := map[frozenKey]float64{}
	var frozenPaid float64
	for _, sl := range slots {
		k := frozenKey{sl.TA, sl.YearMonth, track}
		if _, ok := frozen[k]; !ok {
			live = append(live, sl)
			continue
		}
		if _, seen := lockedBaht[k]; !seen {
			frozenPaid += frozen[k].paid
		}
		lockedBaht[k] += sl.Baht
		locked = append(locked, sl)
	}
	if len(locked) == 0 {
		return settleTrack(mode, track, cap, committed, slots)
	}
	liveCap := cap
	if cap > 0 {
		// Never 0: settleTrack reads a non-positive cap as "unconfigured, so
		// unlimited" — the opposite of a pool the locked months used up.
		liveCap = math.Max(0.01, cap-frozenPaid)
	}
	out := settleTrack(mode, track, liveCap, committed, live)
	for i := range locked {
		k := frozenKey{locked[i].TA, locked[i].YearMonth, track}
		if b := lockedBaht[k]; b > 0 {
			locked[i].PaidBaht = math.Min(locked[i].Baht, frozen[k].paid*locked[i].Baht/b)
		}
	}
	all := append(append([]SlotSettlement{}, out.Slots...), locked...)
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Date != all[j].Date {
			return all[i].Date < all[j].Date
		}
		return all[i].StartTime < all[j].StartTime
	})
	return finishTrack(TrackSettlement{Track: track, Cap: cap, Committed: committed, Slots: all})
}

// MonthFigures is what one claim pack printed per (TA, month, track): the
// ledger rows the locking download writes for the cells it locks.
type MonthFigures map[frozenKey]frozenFigure

// computeMonthFigures prices the slice exactly as the pack does (the same
// costs, the same settlement) and keeps the per-cell totals.
func (s *ExportService) computeMonthFigures(ctx context.Context, courseID uuid.UUID, months []string) (MonthFigures, error) {
	var pr PayRate
	if err := s.pool.QueryRow(ctx, `
		SELECT undergrad_regular, undergrad_special, graduate_regular_hourly, ug_special_monthly_cap
		FROM `+payRatesInForce).Scan(
		&pr.UndergradRegular, &pr.UndergradSpecial, &pr.GraduateRegularHourly, &pr.UGSpecialMonthlyCap); err != nil {
		return nil, err
	}
	costs, err := s.claimCostByTASlot(ctx, courseID, pr, mergedSittingsCTE)
	if err != nil {
		return nil, err
	}
	settlement, err := s.SettleCourse(ctx, courseID)
	if err != nil {
		return nil, err
	}
	_, inSlice, err := s.courseMonthShare(ctx, courseID, months)
	if err != nil {
		return nil, err
	}
	out := MonthFigures{}
	for _, c := range costs {
		if !inSlice(c.YearMonth) {
			continue
		}
		t := settlement.Regular
		if c.Track == "special" {
			t = settlement.Special
		}
		k := frozenKey{c.TA, c.YearMonth, c.Track}
		f := out[k]
		f.earned += c.Baht
		f.paid += c.Baht * t.fundedShare(c.TA, c.Date, c.StartTime)
		out[k] = f
	}
	for k, f := range out {
		out[k] = frozenFigure{earned: round2(f.earned), paid: round2(math.Min(f.paid, f.earned))}
	}
	return out, nil
}

// writeMonthLedger records figs for the cells an export just locked. Runs in
// the locking transaction, so a cell is never exported without its figure.
func writeMonthLedger(ctx context.Context, tx pgx.Tx, actor, courseID uuid.UUID,
	cells []lockedCell, figs MonthFigures) error {
	if len(figs) == 0 {
		return nil
	}
	for _, c := range cells {
		for _, track := range []string{"regular", "special"} {
			f, ok := figs[frozenKey{c.taID, c.gregYM, track}]
			if !ok {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO export_month_ledger
				    (teaching_course_id, ta_id, year_month, track, earned_baht, paid_baht, frozen_by)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				courseID, c.taID, c.gregYM, track, f.earned, f.paid, actor); err != nil {
				return err
			}
		}
	}
	return nil
}

// lockedCell is one (TA, month) an export locked.
type lockedCell struct {
	taID, periodID uuid.UUID
	gregYM         string
}

// sliceCell is one (TA, month) of a course slice that has approved work.
type sliceCell struct {
	taID       uuid.UUID
	taName     string
	gregYM     string
	status     string
	exportedAt *time.Time
	ledger     bool // a ledger figure is in force for it
}

func (c sliceCell) locked() bool { return c.status == "exported" || c.status == "finance_sent" }

// sliceCells lists the (TA, month) cells of a slice that carry approved work,
// with their lock state and whether the ledger covers them. Same population as
// the export gate (non-dropped assignments, grad-special excluded).
func (s *ExportService) sliceCells(ctx context.Context, courseID uuid.UUID, months []string) ([]sliceCell, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.ta_id,
		       COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		       COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,''),
		       sp.year_month, COALESCE(st.status,'pending'), st.exported_at
		FROM teaching_courses tc
		JOIN academic_terms trm ON trm.id = tc.term_id
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		JOIN sections sec ON sec.teaching_course_id = tc.id
		JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		JOIN work_logs wl ON wl.assignment_id = a.id AND wl.status = 'approved'
		 AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		LEFT JOIN submission_period_status st
		  ON st.submission_period_id = sp.id AND st.ta_id = a.ta_id AND st.teaching_course_id = tc.id
		WHERE tc.id = $1
		  AND `+monthFilterSQL("wl.work_date", "$2")+`
		  AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		GROUP BY a.ta_id, tp.prefix, u.title, u.first_name, u.last_name,
		         sp.year_month, st.status, st.exported_at`, courseID, months)
	if err != nil {
		return nil, err
	}
	var out []sliceCell
	for rows.Next() {
		var c sliceCell
		var periodYM string
		if err := rows.Scan(&c.taID, &c.taName, &periodYM, &c.status, &c.exportedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if c.gregYM, err = gregorianYearMonth(periodYM); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	frozen, err := loadFrozenMonths(ctx, s.pool, courseID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		for _, track := range []string{"regular", "special"} {
			if _, ok := frozen[frozenKey{out[i].taID, out[i].gregYM, track}]; ok {
				out[i].ledger = true
			}
		}
	}
	return out, nil
}

// ArchivedExport describes the document already issued for a fully locked
// slice — what a re-download hands back, and the total the preview shows for
// months whose per-TA figures predate the ledger.
type ArchivedExport struct {
	BatchID     uuid.UUID `json:"batch_id"`
	FileName    string    `json:"file_name"`
	TotalBaht   float64   `json:"total_baht"`
	GeneratedAt string    `json:"generated_at"`
	Version     int       `json:"version"`
	// FileAvailable: the ZIP itself is in storage, so a re-download is that
	// exact file rather than a rebuild.
	FileAvailable bool `json:"file_available"`
	// Legacy: some locked month was exported before per-TA figures were
	// recorded (migration 0140). Its live recomputation is not authoritative;
	// the archived total is.
	Legacy bool `json:"legacy"`
}

// archivedForSlice finds the document issued for exactly this slice, if the
// slice is fully locked and that document is current: generated no earlier
// than the latest lock of any of its cells (a month sent back and re-exported
// on another slice makes an older document stale). nil when the slice still
// has something to lock, or no current document exists.
func (s *ExportService) archivedForSlice(ctx context.Context, courseID uuid.UUID, months []string) (*ArchivedExport, error) {
	if len(months) == 0 {
		return nil, nil
	}
	cells, err := s.sliceCells(ctx, courseID, months)
	if err != nil {
		return nil, err
	}
	if len(cells) == 0 {
		return nil, nil
	}
	var lastLock time.Time
	legacy := false
	for _, c := range cells {
		if !c.locked() {
			return nil, nil
		}
		if c.exportedAt != nil && c.exportedAt.After(lastLock) {
			lastLock = *c.exportedAt
		}
		if !c.ledger {
			legacy = true
		}
	}
	var a ArchivedExport
	var generated time.Time
	err = s.pool.QueryRow(ctx, `
		SELECT id, file_name, total_baht::float8, generated_at, version, file_path <> ''
		FROM export_batches
		WHERE teaching_course_id = $1
		  AND months IS NOT NULL
		  AND months @> $2::text[] AND months <@ $2::text[]
		  AND generated_at >= $3
		ORDER BY generated_at DESC
		LIMIT 1`, courseID, months, lastLock).Scan(
		&a.BatchID, &a.FileName, &a.TotalBaht, &generated, &a.Version, &a.FileAvailable)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.GeneratedAt = generated.UTC().Format(time.RFC3339)
	a.Legacy = legacy
	return &a, nil
}

// ArchivedCopy is a re-download served from storage instead of a rebuild.
type ArchivedCopy struct {
	Batch ArchivedExport
	Body  []byte
}

// ArchivedReissue answers a download of a slice that is already exported and
// not sent back: the exact ZIP finance received, byte for byte. Re-pricing it
// could only ever produce a different document under the same name — the
// reason a plain re-download used to be refused with 409.
//
// nil (and no error) means "not a plain re-download, or no file kept": the
// caller builds as usual. A rebuild of a locked slice prices its months from
// the ledger, so it still reproduces the issued figures.
func (s *ExportService) ArchivedReissue(ctx context.Context, courseID uuid.UUID, months []string) (*ArchivedCopy, error) {
	a, err := s.archivedForSlice(ctx, courseID, months)
	if err != nil || a == nil || !a.FileAvailable || s.store == nil {
		return nil, err
	}
	var path string
	if err := s.pool.QueryRow(ctx, `SELECT file_path FROM export_batches WHERE id = $1`, a.BatchID).Scan(&path); err != nil {
		return nil, err
	}
	rc, err := s.store.Open(path)
	if err != nil {
		// The row survives a lost file; rebuilding is the honest fallback.
		return nil, nil
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return &ArchivedCopy{Batch: *a, Body: body}, nil
}

// assertLockedFiguresHold is the drift check, per TA rather than per course.
//
// For a TA whose every month in the slice is locked with a ledger figure, the
// rebuilt document must pay exactly the frozen sum (the ledger hooks make this
// hold by construction; this is the guard that they did). Another TA's new
// hours in an unlocked month move neither figure, so they cannot block it —
// the old course-total comparison refused a correction export of one TA
// because a different TA had changed.
//
// Months locked before the ledger existed have no per-TA figure. For a
// re-download of exactly such a slice with no archived file to serve, the
// course total is all there is to compare: refused if it moved, since the
// rebuilt document would not be the one finance holds.
func (s *ExportService) assertLockedFiguresHold(ctx context.Context, courseID uuid.UUID, months []string, comp *exportComputation) error {
	if len(months) == 0 {
		return nil
	}
	cells, err := s.sliceCells(ctx, courseID, months)
	if err != nil {
		return err
	}
	frozen, err := loadFrozenMonths(ctx, s.pool, courseID)
	if err != nil {
		return err
	}
	inSlice := map[string]bool{}
	for _, m := range months {
		inSlice[m] = true
	}
	type taState struct{ all, frozen int }
	byTA := map[uuid.UUID]*taState{}
	allLocked, anyLegacy := len(cells) > 0, false
	for _, c := range cells {
		st := byTA[c.taID]
		if st == nil {
			st = &taState{}
			byTA[c.taID] = st
		}
		st.all++
		if c.locked() && c.ledger {
			st.frozen++
		}
		if !c.locked() {
			allLocked = false
		} else if !c.ledger {
			anyLegacy = true
		}
	}
	lumpHolders, err := gradSpecialHolderIDs(ctx, s.pool, courseID)
	if err != nil {
		return err
	}
	isHolder := map[uuid.UUID]bool{}
	for _, id := range lumpHolders {
		isHolder[id] = true
	}
	var moved []string
	for _, r := range comp.records {
		st := byTA[r.taID]
		// The lump has its own ledger (0119) and its own guard.
		if st == nil || st.all == 0 || st.frozen != st.all || isHolder[r.taID] {
			continue
		}
		var want float64
		for k, f := range frozen {
			if k.ta == r.taID && inSlice[k.ym] {
				want += f.paid
			}
		}
		if math.Abs(round2(want)-r.actualPaid) > 0.05 {
			moved = append(moved, fmt.Sprintf("%s (เดิม %.2f บาท ปัจจุบัน %.2f บาท)", r.fullName, want, r.actualPaid))
		}
	}
	if len(moved) > 0 {
		return Conflict("ยอดเงินของเดือนที่ส่งออกไปแล้วไม่ตรงกับที่บันทึกไว้ตอนส่งออก: " + strings.Join(moved, ", ") +
			" เอกสารชุดนี้จึงไม่ถูกสร้าง กรุณาแจ้งผู้ดูแลระบบ")
	}
	if !allLocked || !anyLegacy {
		return nil
	}
	var prevTotal float64
	err = s.pool.QueryRow(ctx, `
		SELECT total_baht::float8 FROM export_batches
		WHERE teaching_course_id = $1 AND months IS NOT NULL
		  AND months @> $2::text[] AND months <@ $2::text[]
		ORDER BY generated_at DESC LIMIT 1`, courseID, months).Scan(&prevTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var now float64
	for _, r := range comp.records {
		now += r.actualPaid
	}
	now = round2(now)
	if math.Abs(now-prevTotal) > 0.005 {
		return Conflict(fmt.Sprintf(
			"เดือนที่เลือกส่งออกไปก่อนที่ระบบจะบันทึกยอดรายคนไว้ และไม่พบไฟล์เดิมในระบบจัดเก็บ "+
				"ยอดที่คำนวณใหม่ (%.2f บาท) ไม่ตรงกับเอกสารเดิม (%.2f บาท) จึงออกซ้ำไม่ได้ "+
				"หากต้องการออกเอกสารฉบับแก้ไข ให้ตีกลับเดือนดังกล่าวก่อน แล้วส่งออกใหม่", now, prevTotal))
	}
	return nil
}
