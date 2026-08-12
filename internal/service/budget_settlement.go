package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// budget_settlement.go decides which WORK of a course gets paid when it costs
// more than the course's budget.
//
// Until 04/08/2026 the answer was pro-rata: everyone scaled down by the same
// factor at export time. Two things were wrong with it. The lecturer could not
// approve at all once the cap was reached — Approve refused outright, so work
// that had already happened had no way into the system. And the scaling itself
// was invisible: it ran at export, and the resulting figure could not be derived
// from the claim form, which multiplies hours × rate with no factor in it.
//
// The rule now (staff decision, 04/08/2026):
//
//	คาบ are paid in chronological order until the budget runs out. A คาบ that
//	does not fit is not paid, and neither is anything after it.
//
// A "คาบ" here is a SLOT — one (date, start time) across the whole course — so
// every TA who taught the same hour is paid or not paid together. That is what
// makes the outcome defensible: nothing on the claim form can show two people
// treated differently for the same class.
//
// It was whole MONTHS until 04/08/2026, and the waste was severe: a pool that
// could not afford the next month dropped all of it, so a 4,000฿ pool facing
// months of 400/800/3,600 paid only 1,200 and left 2,800 — 70% of the budget —
// unclaimed. Cutting at the คาบ instead leaves under one slot's cost behind.
//
// Still no scaling, and still no skipping ahead: once one คาบ fails to fit,
// every later one is unpaid even if it were cheap enough. Paying a November
// class but not an October one is impossible to explain to somebody who taught
// both, and it would make the outcome depend on the shape of the timetable
// rather than on the calendar.

// SlotSettlement is one (date, start time) of one pool: what that คาบ costs
// across every TA who taught it, and whether the budget reached it.
type SlotSettlement struct {
	Date      string  `json:"date"`       // "2026-10-20"
	StartTime string  `json:"start_time"` // "15:00"
	YearMonth string  `json:"year_month"` // "2026-10", Gregorian — for rollups
	Baht      float64 `json:"baht"`
	Paid      bool    `json:"paid"`
}

// MonthSettlement is one month of one pool: what it costs and whether the
// budget reached it.
type MonthSettlement struct {
	YearMonth string  `json:"year_month"` // "2026-06", Gregorian
	Baht      float64 `json:"baht"`
	PaidBaht  float64 `json:"paid_baht"`
	// Paid means the month was covered IN FULL. With a คาบ-level cutoff one
	// month per pool can land in between — see PaidBaht.
	Paid bool `json:"paid"`
}

// TrackSettlement is one budget pool's outcome. The pools are separate by
// ประกาศ — regular-track work draws only on the regular budget — so a course
// with both tracks can have two different cutoff months.
type TrackSettlement struct {
	Track string  `json:"track"` // "regular" | "special"
	Cap   float64 `json:"cap"`
	// Committed is spending that is not monthly and is taken off the top:
	// the graduate-special lump sum, which is a flat term figure.
	Committed float64 `json:"committed"`
	// Months is the rollup the screens read; Slots is what the cutoff actually
	// ran on and what the printed claim is filtered by.
	Slots       []SlotSettlement  `json:"-"`
	Months      []MonthSettlement `json:"months"`
	PaidBaht    float64           `json:"paid_baht"`
	DroppedBaht float64           `json:"dropped_baht"`
	// CutoffMonth is the first month not covered in full, "" when everything
	// fits. CutoffDate/CutoffStart pin the exact คาบ the money stopped at —
	// everything from there on is unpaid, which is what filters the document.
	CutoffMonth string `json:"cutoff_month,omitempty"`
	CutoffDate  string `json:"cutoff_date,omitempty"`
	CutoffStart string `json:"cutoff_start,omitempty"`
}

// unpaidFrom reports whether a คาบ at this (date, start) falls on the unpaid
// side of the cutoff. The cutoff is chronological, so "unpaid" is always a
// suffix in time — one comparison, no set membership.
func (t TrackSettlement) unpaidFrom(date, start string) bool {
	if t.CutoffDate == "" {
		return false
	}
	if date != t.CutoffDate {
		return date > t.CutoffDate
	}
	return start >= t.CutoffStart
}

// CourseSettlement answers "what will actually be paid, and what falls off".
type CourseSettlement struct {
	Regular TrackSettlement `json:"regular"`
	Special TrackSettlement `json:"special"`
	// UnpaidMonths is the union across pools, ascending — what the screens
	// name to the lecturer and the TA.
	UnpaidMonths []string `json:"unpaid_months,omitempty"`
	// PartialMonths get some of their คาบ paid and some not — possible only
	// since the cutoff moved off the month boundary. Named separately because
	// "ได้บางส่วน" and "ไม่ได้เลย" are different sentences to a TA.
	PartialMonths []string `json:"partial_months,omitempty"`
	DroppedBaht   float64  `json:"dropped_baht"`
	OverBudget    bool     `json:"over_budget"`
	// SpilledBaht is capacity the special pool lent to the regular one under the
	// concurrent-section rule. Reported so a course that only balances because
	// of the spill does not look like it simply fitted.
	SpilledBaht float64 `json:"spilled_baht,omitempty"`
}

// settleTrack walks one pool's คาบ in order and marks where the money stops.
func settleTrack(track string, cap, committed float64, slots []SlotSettlement) TrackSettlement {
	out := TrackSettlement{Track: track, Cap: cap, Committed: committed, Slots: slots}
	// A cap of 0 means "not configured" rather than "no money" — the student
	// count has not been entered yet, and refusing to pay anything on that basis
	// would be a silent zeroing. Treated as unlimited; the export's own
	// student-count gate is what stops a course in that state.
	unlimited := cap <= 0
	remaining := cap - committed
	stopped := false
	for i := range out.Slots {
		sl := &out.Slots[i]
		if !stopped && (unlimited || sl.Baht <= remaining+0.01) {
			sl.Paid = true
			out.PaidBaht += sl.Baht
			remaining -= sl.Baht
			continue
		}
		// This คาบ does not fit, so it and everything after it are unpaid.
		// Not "skip it and try the next, cheaper one": paying a later class but
		// not an earlier one would be impossible to explain to the person who
		// taught both.
		if out.CutoffDate == "" {
			out.CutoffDate, out.CutoffStart = sl.Date, sl.StartTime
			out.CutoffMonth = sl.YearMonth
		}
		stopped = true
		sl.Paid = false
		out.DroppedBaht += sl.Baht
	}
	out.PaidBaht = round2(out.PaidBaht)
	out.DroppedBaht = round2(out.DroppedBaht)
	out.Months = rollUpMonths(out.Slots)
	return out
}

// rollUpMonths turns the คาบ ledger back into the per-month view the screens
// and the shortfall notice speak in.
func rollUpMonths(slots []SlotSettlement) []MonthSettlement {
	idx := map[string]int{}
	var out []MonthSettlement
	for _, sl := range slots {
		i, ok := idx[sl.YearMonth]
		if !ok {
			i = len(out)
			idx[sl.YearMonth] = i
			out = append(out, MonthSettlement{YearMonth: sl.YearMonth})
		}
		out[i].Baht += sl.Baht
		if sl.Paid {
			out[i].PaidBaht += sl.Baht
		}
	}
	for i := range out {
		out[i].Baht = round2(out[i].Baht)
		out[i].PaidBaht = round2(out[i].PaidBaht)
		out[i].Paid = out[i].PaidBaht >= out[i].Baht-0.01
	}
	sort.Slice(out, func(i, j int) bool { return out[i].YearMonth < out[j].YearMonth })
	return out
}

// SettleCourse prices a course's APPROVED months and applies the cutoff. This
// is what the export pays.
func (s *ExportService) SettleCourse(ctx context.Context, courseID uuid.UUID) (*CourseSettlement, error) {
	return s.settle(ctx, courseID, mergedSittingsCTE)
}

// ForecastCourse asks the same question of everything still in play — approved,
// submitted and draft.
//
// This is what the warnings read. Settled figures can only ever say "the money
// has already run out"; by then the lecturer has approved work that will not be
// paid and nobody could have known. The forecast crosses the line first, which
// is the entire reason the warning exists.
//
// It reads logged rows rather than projecting from the timetable: the term is
// generated up front from section_schedules, so the future months usually exist
// as drafts already. A TA who has not generated yet is under-counted — the
// forecast is a floor, never an over-statement, so it does not cry wolf.
func (s *ExportService) ForecastCourse(ctx context.Context, courseID uuid.UUID) (*CourseSettlement, error) {
	return s.settle(ctx, courseID, mergedSittingsForecastCTE)
}

func (s *ExportService) settle(ctx context.Context, courseID uuid.UUID, sittingsCTE string) (*CourseSettlement, error) {
	var pr PayRate
	if err := s.pool.QueryRow(ctx, `
		SELECT undergrad_regular, undergrad_special, graduate_regular_hourly,
		       graduate_special_lumpsum, grad_special_term_cap, term_months
		FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&pr.UndergradRegular, &pr.UndergradSpecial, &pr.GraduateRegularHourly,
		&pr.GraduateSpecialLumpsum, &pr.GradSpecialTermCap, &pr.TermMonths); err != nil {
		return nil, err
	}
	var capRegular, capSpecial float64
	if s.budget != nil {
		if snap, err := s.budget.Compute(ctx, courseID); err == nil {
			capRegular, capSpecial = snap.TermPayRegular, snap.TermPaySpecial
		}
	}

	// Cost per (month, pool) under the SAME pricing the claim document prints —
	// merged sittings, B2 overlap off the special side, monthly cap. Settling on
	// any other number invents a shortfall the document doesn't have.
	costs, err := s.claimCostByTASlot(ctx, courseID, pr, sittingsCTE)
	if err != nil {
		return nil, err
	}
	byTrack := map[string][]SlotSettlement{
		"regular": slotLedger(costs, "regular"),
		"special": slotLedger(costs, "special"),
	}

	// Graduate-special is a flat term lump, not monthly, so it cannot be cut by
	// month — it is committed off the top of the special pool. A grad-special TA
	// either holds the appointment or does not — eligibility is just an approved
	// assignment; grad-special TAs no longer log work_logs at all (the system
	// computes their pay automatically from the regular track's class schedule),
	// so there is nothing left to gate on there.
	var gradSpecialTAs int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT a.ta_id)
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec ON sec.id = a.section_id AND sec.track = 'special'
		JOIN users u ON u.id = a.ta_id
		WHERE sec.teaching_course_id = $1
		  AND a.level::text IN ('master','phd')`,
		courseID).Scan(&gradSpecialTAs); err != nil {
		return nil, err
	}
	// graduate_special_lumpsum IS the whole-term-per-course figure (2026 meeting
	// correction), not a monthly rate — do not multiply by termMonths.
	gradLump := pr.GraduateSpecialLumpsum
	if pr.GradSpecialTermCap > 0 && gradLump > pr.GradSpecialTermCap {
		gradLump = pr.GradSpecialTermCap
	}
	committedSpecial := gradLump * float64(gradSpecialTAs)

	// Concurrent-section spill (B2, ประกาศ): when the regular pool runs out, the
	// regular-rate pay for hours a TA worked on BOTH tracks at the same clock
	// time may draw on whatever the special pool has not used —
	// "เบิกภาคปกติก่อน ถ้างบปกติหมดค่อยไหลไปพิเศษ". Only those overlap hours may
	// borrow; ordinary regular work never touches the special budget.
	//
	// Carried over from the pro-rata version it replaced, as a transfer of
	// CAPACITY made before the months are cut. Special's spare room is measured
	// against its whole cost rather than its post-cutoff spend: a pool that is
	// itself short has nothing to lend, which is what the old rule said too.
	spill := 0.0
	if capRegular > 0 && capSpecial > 0 {
		spillable, serr := s.spillableRegularBaht(ctx, courseID, pr, sittingsCTE)
		if serr != nil {
			return nil, serr
		}
		var totalRegular, totalSpecial float64
		for _, sl := range byTrack["regular"] {
			totalRegular += sl.Baht
		}
		for _, sl := range byTrack["special"] {
			totalSpecial += sl.Baht
		}
		spill = spillAllowance(capRegular, capSpecial, committedSpecial,
			totalRegular, totalSpecial, spillable)
	}

	// Never let the lend take the special cap to 0 — settleTrack reads a
	// non-positive cap as "unconfigured, so unlimited", the exact opposite of a
	// pool that has just given everything away.
	specialLeft := math.Max(0.01, capSpecial-spill)
	if capSpecial <= 0 {
		specialLeft = capSpecial // genuinely unconfigured; leave it that way
	}
	out := &CourseSettlement{
		Regular: settleTrack("regular", capRegular+spill, 0, byTrack["regular"]),
		Special: settleTrack("special", specialLeft, committedSpecial, byTrack["special"]),
	}
	out.SpilledBaht = round2(spill)
	out.DroppedBaht = round2(out.Regular.DroppedBaht + out.Special.DroppedBaht)
	out.OverBudget = out.DroppedBaht > 0

	// "ไม่ได้เลย" and "ได้บางส่วน" are separated here rather than at the screen:
	// a month that lost one คาบ and one that lost all of them are different news
	// for the person who worked them, and only the settlement knows which is
	// which. A month partly paid on one pool but wholly unpaid on the other
	// counts as partial — some of that month's money did arrive.
	nothing, some := map[string]bool{}, map[string]bool{}
	for _, t := range []TrackSettlement{out.Regular, out.Special} {
		for _, m := range t.Months {
			switch {
			case m.Baht <= 0:
			case m.PaidBaht <= 0.01:
				nothing[m.YearMonth] = true
			case !m.Paid:
				some[m.YearMonth] = true
			}
		}
	}
	for ym := range nothing {
		if some[ym] {
			continue
		}
		out.UnpaidMonths = append(out.UnpaidMonths, ym)
	}
	for ym := range some {
		out.PartialMonths = append(out.PartialMonths, ym)
	}
	sort.Strings(out.UnpaidMonths)
	sort.Strings(out.PartialMonths)
	return out, nil
}

// EarnedBaht is what the course's work is worth under the claim rules, before
// the budget cutoff. PaidBaht is what survives it. Every screen that shows one
// number against the budget reads these, so the list, the preview and the
// printed document cannot quote three different totals for the same hours —
// which is exactly what happened until 04/08/2026, when the payout list summed
// raw hours × rate on its own and put SC362102 at 24,360 against a preview
// showing 14,860.
func (c *CourseSettlement) EarnedBaht() float64 {
	if c == nil {
		return 0
	}
	return round2(c.Regular.PaidBaht + c.Regular.DroppedBaht +
		c.Special.PaidBaht + c.Special.DroppedBaht + c.Special.Committed)
}

// PaidBaht includes Committed: the graduate-special lump is taken off the top
// of the pool, not cut by คาบ, so a holder keeps it.
func (c *CourseSettlement) PaidBaht() float64 {
	if c == nil {
		return 0
	}
	return round2(c.Regular.PaidBaht + c.Special.PaidBaht + c.Special.Committed)
}

// unpaidMonthSet is the lookup the export pricing needs: months that must be
// billed at zero, per pool.
func (c *CourseSettlement) unpaidMonthSet(track string) map[string]bool {
	t := c.Regular
	if track == "special" {
		t = c.Special
	}
	out := map[string]bool{}
	for _, m := range t.Months {
		if m.PaidBaht <= 0.01 && m.Baht > 0 {
			out[m.YearMonth] = true
		}
	}
	return out
}

// dropUnpaidWork rewrites each TA's actualPaid to cover only the คาบ the budget
// reached.
//
// payBaht stays as earned — the two figures are what the preview compares, and
// the gap between them is precisely the money the cutoff took away. Recomputed
// per TA from their own สittings rather than scaled from the total, because
// scaling is the thing this rule exists to avoid.
// months (Gregorian "YYYY-MM", empty = all) must match the scope the records
// were built with: a มิ.ย.–ก.ย. document never contained October's คาบ, so
// deducting October's dropped ones from it would understate the slice.
func (s *ExportService) dropUnpaidWork(
	ctx context.Context, courseID uuid.UUID,
	records []exportRow, settlement *CourseSettlement, pr PayRate, months []string,
) error {
	if settlement.Regular.CutoffDate == "" && settlement.Special.CutoffDate == "" {
		return nil
	}

	// Per (TA, คาบ, track) cost from the SAME source SettleCourse settled on —
	// what a cut คาบ removes must be exactly what the settlement counted.
	costs, err := s.claimCostByTASlot(ctx, courseID, pr, mergedSittingsCTE)
	if err != nil {
		return err
	}
	_, inSlice, err := s.courseMonthShare(ctx, courseID, months)
	if err != nil {
		return err
	}
	lost := map[uuid.UUID]float64{}
	for _, c := range costs {
		if !inSlice(c.YearMonth) {
			continue
		}
		t := settlement.Regular
		if c.Track == "special" {
			t = settlement.Special
		}
		if t.unpaidFrom(c.Date, c.StartTime) {
			lost[c.TA] += c.Baht
		}
	}

	for i := range records {
		if drop := lost[records[i].taID]; drop > 0 {
			// Clamp at zero: the grad-special lump is not คาบ-scoped, so a TA
			// holding one keeps it even when every hourly คาบ falls off.
			v := round2(records[i].actualPaid - drop)
			if v < 0 {
				v = 0
			}
			records[i].actualPaid = v
		}
	}
	return nil
}

// SettlementView pairs what the budget has already committed to with what it
// will owe if everything currently logged gets approved.
//
// Both, not one: "ยังพอ" and "จะไม่พอ" are different sentences and the reader
// needs to know which one they are being told.
type SettlementView struct {
	Committed *CourseSettlement `json:"committed"`
	Forecast  *CourseSettlement `json:"forecast"`
}

// SettlementForViewer is the settlement as a lecturer or TA may read it.
//
// Same figures the export uses — deliberately one source, so the warning the
// lecturer sees names the months the export will actually drop. Anyone teaching
// or assisting the course may read it: the budget is not confidential to them,
// and it is their own pay it decides.
func (s *ExportService) SettlementForViewer(
	ctx context.Context, actor, courseID uuid.UUID, privileged bool,
) (*SettlementView, error) {
	if !privileged {
		var allowed bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM teaching_lecturers tl
			                WHERE tl.teaching_course_id = $1 AND tl.lecturer_id = $2)
			    OR EXISTS (SELECT 1 FROM ta_request_assignments a
			               JOIN sections sec ON sec.id = a.section_id
			                WHERE sec.teaching_course_id = $1 AND a.ta_id = $2
			                  AND a.state <> 'dropped')`,
			courseID, actor).Scan(&allowed); err != nil {
			return nil, err
		}
		if !allowed {
			return nil, ErrForbidden
		}
	}
	committed, err := s.SettleCourse(ctx, courseID)
	if err != nil {
		return nil, err
	}
	forecast, err := s.ForecastCourse(ctx, courseID)
	if err != nil {
		return nil, err
	}
	return &SettlementView{Committed: committed, Forecast: forecast}, nil
}

// budgetShortfallKey is what a course was last warned about — the cutoff months
// of both pools, joined. Comparing it to the stored one answers "is this new?"
func budgetShortfallKey(c *CourseSettlement) string {
	if c == nil || !c.OverBudget {
		return ""
	}
	// The คาบ cutoff, not just the months: a shortfall that grows from "half of
	// October" to "all of October" is news even though the month list is
	// unchanged, and the officer should hear about it again.
	return strings.Join(c.UnpaidMonths, ",") + "|" + strings.Join(c.PartialMonths, ",") +
		"|" + c.Regular.CutoffDate + c.Regular.CutoffStart +
		"|" + c.Special.CutoffDate + c.Special.CutoffStart
}

// NotifyBudgetShortfall tells the course's lecturers and TAs when the money
// stops reaching the end of the term — once, and again only if the answer
// changes.
//
// Driven off the FORECAST, not the settled figure: by the time settled spending
// crosses the line, hours have already been approved that will not be paid.
//
// Best-effort by design. A warning that failed to send must never be the reason
// an approval fails — the approval is the real work, this is a courtesy on top
// of it, and the screens carry the same information anyway.
func (s *ExportService) NotifyBudgetShortfall(ctx context.Context, courseID uuid.UUID) {
	if s.notify == nil {
		return
	}
	forecast, err := s.ForecastCourse(ctx, courseID)
	if err != nil {
		return
	}
	key := budgetShortfallKey(forecast)

	var last *string
	err = s.pool.QueryRow(ctx,
		`SELECT last_cutoff FROM course_budget_alerts WHERE teaching_course_id = $1`,
		courseID).Scan(&last)
	known := ""
	if err == nil && last != nil {
		known = *last
	}
	if key == known {
		return // nothing new to say
	}

	var stored *string
	if key != "" {
		stored = &key
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO course_budget_alerts (teaching_course_id, last_cutoff, notified_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (teaching_course_id)
		DO UPDATE SET last_cutoff = EXCLUDED.last_cutoff, notified_at = NOW()`,
		courseID, stored); err != nil {
		return
	}
	// Recovering from a shortfall is worth recording but not worth interrupting
	// anybody for: nobody has to act on good news.
	if key == "" {
		return
	}

	var code, nameTH string
	_ = s.pool.QueryRow(ctx,
		`SELECT code, name_th FROM teaching_courses WHERE id = $1`, courseID).Scan(&code, &nameTH)
	title := "งบไม่พอ " + code
	// Two different sentences: a month that lost some คาบ still pays something,
	// and saying "จะไม่ได้รับค่าตอบแทน" about it would be wrong.
	var what []string
	if len(forecast.PartialMonths) > 0 {
		what = append(what, "เดือน "+strings.Join(thaiMonthLabels(forecast.PartialMonths), ", ")+" ได้ไม่ครบทุกคาบ")
	}
	if len(forecast.UnpaidMonths) > 0 {
		what = append(what, "เดือน "+strings.Join(thaiMonthLabels(forecast.UnpaidMonths), ", ")+" ไม่ได้รับค่าตอบแทน")
	}
	body := fmt.Sprintf(
		"%s %s\nงบรายวิชาไม่พอจ่ายทั้งหมด %s (รวม %.0f บาท)\n"+
			"ชั่วโมงยังถูกบันทึกไว้ครบ และอาจารย์ยังอนุมัติได้ตามปกติ",
		code, nameTH, strings.Join(what, " และ"), forecast.DroppedBaht)

	if lects, err := courseLecturerIDs(ctx, s.pool, courseID); err == nil {
		for _, id := range lects {
			s.notify.Send(ctx, id, title, body,
				"/lecturer/courses/"+courseID.String()+"/reports")
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.ta_id
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		WHERE sec.teaching_course_id = $1 AND a.state <> 'dropped'`, courseID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ta uuid.UUID
		if err := rows.Scan(&ta); err != nil {
			return
		}
		s.notify.Send(ctx, ta, title, body,
			"/ta/courses/"+courseID.String()+"/worklog")
	}
}

// thaiMonthLabels turns "2026-09" into "กันยายน 2569" for the message body.
// thaiMonthLabelsBE is the same for keys that are ALREADY Buddhist-era —
// submission_periods.year_month is stored as "2569-08", not "2026-08". Passing
// one of those to thaiMonthLabels prints "สิงหาคม 3112", because it adds 543 to
// a year that already had it.
func thaiMonthLabelsBE(yms []string) []string {
	out := make([]string, 0, len(yms))
	for _, ym := range yms {
		year, month, ok := strings.Cut(ym, "-")
		m, err := strconv.Atoi(month)
		if !ok || err != nil || m < 1 || m > 12 {
			out = append(out, ym)
			continue
		}
		out = append(out, thaiMonthNames[m]+" "+year)
	}
	return out
}

func thaiMonthLabels(yms []string) []string {
	out := make([]string, 0, len(yms))
	for _, ym := range yms {
		t, err := time.Parse("2006-01", ym)
		if err != nil {
			out = append(out, ym)
			continue
		}
		out = append(out, fmt.Sprintf("%s %d", thaiMonthNames[int(t.Month())], t.Year()+543))
	}
	return out
}

// spillAllowance is how much of the special pool the regular one may borrow.
//
// Three separate ceilings, all of which must hold: the regular side cannot take
// more than it is short, the special side cannot lend more than it has left
// over, and only the concurrent-section pay (spillable) is entitled to borrow
// at all. A pool that is itself over budget lends nothing — its "unused" goes
// negative and the minimum clamps to zero.
func spillAllowance(capRegular, capSpecial, committedSpecial, totalRegular, totalSpecial, spillable float64) float64 {
	if capRegular <= 0 || capSpecial <= 0 {
		return 0 // an unset cap means unlimited; there is nothing to rescue
	}
	shortfall := totalRegular - capRegular
	unused := capSpecial - committedSpecial - totalSpecial
	return math.Max(0, math.Min(math.Min(shortfall, unused), spillable))
}

// b2StatusFilter picks the work-log statuses the B2 overlap should see, to
// match the settlement being computed: the export prices approved work, the
// forecast prices everything not yet rejected.
func b2StatusFilter(sittingsCTE string) string {
	if sittingsCTE == mergedSittingsForecastCTE {
		return "w1.status <> 'rejected' AND w2.status <> 'rejected'"
	}
	return "w1.status = 'approved' AND w2.status = 'approved'"
}

// b2OverlapCTE emits the CTE chain computing, per TA per month, the DISTINCT
// clock hours an undergrad worked on a regular AND a special section of course
// $1 at the same time (กติกา B2 — those hours are billed once, on the regular
// side). The pairing is many-to-many, so pair intersections are merged into a
// union per TA per day before summing, exactly as billable_hours.go merges
// co-taught sittings. Exposes b2_overlap(ta_id, ym, hours); meant to be
// appended after another CTE with a leading comma.
func b2OverlapCTE(statusFilter string) string {
	return `
	b2_pairs AS (
	    SELECT a1.ta_id, w1.work_date,
	           GREATEST(w1.start_time, w2.start_time) AS starts_at,
	           LEAST(w1.end_time, w2.end_time)        AS ends_at
	    FROM work_logs w1
	    JOIN ta_request_assignments a1 ON a1.id = w1.assignment_id AND a1.level::text = 'undergrad'
	    JOIN sections s1 ON s1.id = a1.section_id AND s1.track = 'regular'
	    JOIN ta_request_assignments a2 ON a2.ta_id = a1.ta_id
	    JOIN sections s2 ON s2.id = a2.section_id AND s2.track = 'special'
	    JOIN work_logs w2 ON w2.assignment_id = a2.id AND w2.work_date = w1.work_date
	    WHERE s1.teaching_course_id = $1 AND s2.teaching_course_id = $1
	      AND ` + statusFilter + `
	      AND w1.start_time < w2.end_time AND w2.start_time < w1.end_time
	),
	b2_marked AS (
	    SELECT *,
	           CASE WHEN starts_at <= MAX(ends_at) OVER (
	                    PARTITION BY ta_id, work_date
	                    ORDER BY starts_at, ends_at
	                    ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING)
	                THEN 0 ELSE 1 END AS starts_new
	    FROM b2_pairs
	),
	b2_grouped AS (
	    SELECT *,
	           SUM(starts_new) OVER (
	               PARTITION BY ta_id, work_date
	               ORDER BY starts_at, ends_at
	               ROWS UNBOUNDED PRECEDING) AS block
	    FROM b2_marked
	),
	b2_blocks AS (
	    SELECT ta_id, work_date, MIN(starts_at) AS starts_at, MAX(ends_at) AS ends_at
	    FROM b2_grouped GROUP BY ta_id, work_date, block
	),
	b2_overlap AS (
	    SELECT ta_id, to_char(work_date, 'YYYY-MM') AS ym,
	           SUM(EXTRACT(EPOCH FROM (ends_at - starts_at)) / 3600.0) AS hours
	    FROM b2_blocks GROUP BY 1, 2
	)`
}

// spillableRegularBaht is the regular-rate pay for the B2 overlap hours — the
// only money entitled to borrow from the special pool (the spill).
func (s *ExportService) spillableRegularBaht(
	ctx context.Context, courseID uuid.UUID, pr PayRate, sittingsCTE string,
) (float64, error) {
	var hours float64
	if err := s.pool.QueryRow(ctx,
		"WITH"+b2OverlapCTE(b2StatusFilter(sittingsCTE))+`
		SELECT COALESCE(SUM(hours), 0) FROM b2_overlap`,
		courseID).Scan(&hours); err != nil {
		return 0, err
	}
	return hours * pr.UndergradRegular, nil
}

// taSlotCost is one TA's share of one คาบ — a (date, start time) on one track.
type taSlotCost struct {
	TA        uuid.UUID
	Date      string // "2026-10-20"
	StartTime string // "15:00"
	EndTime   string // "17:00"
	YearMonth string // "2026-10"
	Track     string
	// Level is the TA's assignment level on this คาบ: "undergrad", "master",
	// or "phd" — the raw ta_request_assignments.level value, not the
	// two-value printed grouping (see gradLevel below).
	Level string
	Baht  float64
}

// gradLevel is true for any taSlotCost row belonging to a graduate (master or
// phd) assignment — the two-value split every document that separates
// undergrad from graduate money reads instead of comparing Level directly.
func (c taSlotCost) gradLevel() bool {
	return c.Level == "master" || c.Level == "phd"
}

// claimCostByTASlot prices every (TA, คาบ, track) of a course exactly as the
// printed claim does: merged sittings, the B2 overlap removed from the special
// side, and the ป.ตรี-พิเศษ monthly cap. Grad-special is 0 here — the lump is
// term-scoped and handled by its holder.
//
// The settlement's cutoff, the export's pay column, and the dropped-work
// arithmetic ALL read this. Pricing exists once; three copies of it is how
// SC362102 came to show three different totals for the same hours.
//
// The monthly cap is a per-person ceiling on a MONTH, so it cannot be evaluated
// one คาบ at a time. It is applied to the month and then spread across that
// month's คาบ in proportion to what each is worth — the month total stays
// exactly right, and the คาบ keep their relative sizes for the cutoff to walk.
func (s *ExportService) claimCostByTASlot(
	ctx context.Context, courseID uuid.UUID, pr PayRate, sittingsCTE string,
) ([]taSlotCost, error) {
	rows, err := s.pool.Query(ctx, `WITH`+sittingsCTE+`,`+b2OverlapCTE(b2StatusFilter(sittingsCTE))+`,
	sit AS (
	    SELECT ta_id,
	           to_char(work_date, 'YYYY-MM-DD') AS d,
	           to_char(start_time, 'HH24:MI')   AS st,
	           to_char(end_time, 'HH24:MI')     AS et,
	           to_char(work_date, 'YYYY-MM')    AS ym,
	           track, level, SUM(hours) AS hrs
	    FROM sittings
	    WHERE teaching_course_id = $1
	    GROUP BY 1, 2, 3, 4, 5, 6, 7
	),
	-- Raw (uncapped, B2-adjusted) value per คาบ, and the month total it belongs
	-- to, so the cap can be turned into a per-คาบ factor below.
	priced AS (
	    SELECT sit.*,
	           CASE
	               WHEN sit.level = 'undergrad' AND sit.track = 'regular' THEN sit.hrs * $2
	               WHEN sit.level IN ('master','phd') AND sit.track = 'regular' THEN sit.hrs * $4
	               WHEN sit.level = 'undergrad' AND sit.track = 'special' THEN
	                   GREATEST(sit.hrs - COALESCE(o.hours, 0) * (sit.hrs / NULLIF(mo.month_hrs, 0)), 0) * $3
	               ELSE 0
	           END AS raw_baht
	    FROM sit
	    LEFT JOIN (
	        SELECT ta_id, ym, track, SUM(hrs) AS month_hrs
	        FROM sit GROUP BY 1, 2, 3
	    ) mo ON mo.ta_id = sit.ta_id AND mo.ym = sit.ym AND mo.track = sit.track
	    LEFT JOIN b2_overlap o
	           ON o.ta_id = sit.ta_id AND o.ym = sit.ym AND sit.track = 'special'
	)
	SELECT p.ta_id, p.d, p.st, p.et, p.ym, p.track, p.level,
	       CASE
	           WHEN $5::float8 > 0 AND p.level = 'undergrad' AND p.track = 'special'
	                AND tot.month_baht > $5
	           THEN p.raw_baht * ($5 / tot.month_baht)
	           ELSE p.raw_baht
	       END AS baht
	FROM priced p
	JOIN (
	    SELECT ta_id, ym, track, SUM(raw_baht) AS month_baht
	    FROM priced GROUP BY 1, 2, 3
	) tot ON tot.ta_id = p.ta_id AND tot.ym = p.ym AND tot.track = p.track
	ORDER BY p.d, p.st`,
		courseID, pr.UndergradRegular, pr.UndergradSpecial,
		pr.GraduateRegularHourly, pr.UGSpecialMonthlyCap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []taSlotCost
	for rows.Next() {
		var c taSlotCost
		if err := rows.Scan(&c.TA, &c.Date, &c.StartTime, &c.EndTime, &c.YearMonth, &c.Track, &c.Level, &c.Baht); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// slotLedger groups per-TA คาบ costs into the course-wide slots the cutoff
// walks: one entry per (date, start time), ascending, holding what that คาบ
// costs across everybody who taught it.
func slotLedger(costs []taSlotCost, track string) []SlotSettlement {
	type key struct{ d, st string }
	idx := map[key]int{}
	var out []SlotSettlement
	for _, c := range costs {
		if c.Track != track || c.Baht <= 0 {
			continue
		}
		k := key{c.Date, c.StartTime}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, SlotSettlement{Date: c.Date, StartTime: c.StartTime, YearMonth: c.YearMonth})
		}
		out[i].Baht += c.Baht
	}
	for i := range out {
		out[i].Baht = round2(out[i].Baht)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].StartTime < out[j].StartTime
	})
	return out
}
