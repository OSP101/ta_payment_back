package service

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// budget_settlement.go decides how much of a course's work gets paid when it
// costs more than the course's budget.
//
// The rule (college decision, 11/09/2026):
//
//	The pool is divided between the TAs in proportion to what each is owed —
//	as MONEY, not as คาบ. Each person's share is rounded down to a whole baht
//	and that figure is what the claim form's ขอเบิกจ่ายเพียง carries.
//
// Nothing is cut at a คาบ any more. The printed claim already lists every hour
// taught and states the funded figure on one line, so there was never a reason
// for that figure to be a sum of whole คาบ — and making it one had two costs the
// college would not carry (see the probe of 11/09/2026): two TAs with identical
// hours came out a whole คาบ apart, and every pool left change in the account
// that a TA was owed. Proportional money fixes both: equal work is paid equally
// to the baht, and the pool is spent to within the rounding.
//
// What the lecturer still chooses is WHERE in the calendar each person's share
// lands (SettlementMode), because the monthly documents — the per-slice ปะหน้า
// จ่ายตรง and the course summary's เบิกจ่ายเดือน — need a figure per month:
//
//   - chronological: months are paid in full in date order; the month the money
//     runs out in gets what is left; later months get nothing.
//   - spread: every month is cut by the same proportion, so none is paid nothing.
//
// Both place the same per-person total; only its distribution over months
// differs. Inside a month the amount is spread over that month's คาบ in
// proportion to their cost, so per-คาบ figures exist for the slicing arithmetic
// without any คาบ being singled out.
//
// History, for anyone reading an old document: whole months until 04/08/2026,
// whole คาบ course-wide until 07/09/2026, whole คาบ per person until 11/09/2026.

// SlotSettlement is one person's คาบ — one (TA, date, start time) of one pool —
// with what it costs and how much of that the budget funds.
type SlotSettlement struct {
	TA        uuid.UUID `json:"-"`
	Date      string    `json:"date"`       // "2026-10-20"
	StartTime string    `json:"start_time"` // "15:00"
	YearMonth string    `json:"year_month"` // "2026-10", Gregorian — for rollups
	Baht      float64   `json:"baht"`
	// PaidBaht is the funded part of Baht, in [0, Baht]. It is a share of the
	// person's money placed on this คาบ for the month arithmetic, not a verdict
	// on the คาบ: a partly funded month has every คาบ partly funded.
	PaidBaht float64 `json:"paid_baht"`
}

// MonthSettlement is one month of one pool: what it costs and how much of that
// the budget reached.
type MonthSettlement struct {
	YearMonth string  `json:"year_month"` // "2026-06", Gregorian
	Baht      float64 `json:"baht"`
	PaidBaht  float64 `json:"paid_baht"`
	// Paid means the month was covered IN FULL.
	Paid bool `json:"paid"`
}

// PersonSettlement is one TA's outcome inside one pool: what their work in
// each month costs and how much of it the budget funds. This is the figure the
// lecturer is shown per person per month, so the split is never explained from
// a course total that nobody can check against their own payslip.
type PersonSettlement struct {
	TAID  uuid.UUID `json:"ta_id"`
	Name  string    `json:"name"`
	Level string    `json:"level"` // "undergrad" | "master" | "phd"
	// Months carries this person's own monthly figures; Baht/PaidBaht are their
	// sums. Empty for a grad-special lump holder, who has no monthly work.
	Months   []MonthSettlement `json:"months"`
	Baht     float64           `json:"baht"`
	PaidBaht float64           `json:"paid_baht"`
	// LumpBaht is the graduate-special flat term lump this person holds, taken
	// off the top of the special pool — not monthly, not cut.
	LumpBaht float64 `json:"lump_baht,omitempty"`
}

// TrackSettlement is one budget pool's outcome. The pools are separate by
// ประกาศ — regular-track work draws only on the regular budget — so a course
// with both tracks can be short on one and whole on the other.
type TrackSettlement struct {
	Track string  `json:"track"` // "regular" | "special"
	Cap   float64 `json:"cap"`
	// Committed is spending that is taken off the top of the pool before the
	// shares are cut: the graduate-special lump sums, flat term figures.
	Committed float64 `json:"committed"`
	// CommittedByMonth dates the same money: each holder's lump laid on the
	// months by their own logged hours (gradLumpByMonth), summed over holders.
	// The monthly documents read this; Committed alone cannot say which month
	// a lump is paid in.
	CommittedByMonth map[string]float64 `json:"committed_by_month,omitempty"`
	// Months is the rollup the screens read; Slots is what the shares were
	// placed on and what the printed documents sum.
	Slots  []SlotSettlement  `json:"-"`
	Months []MonthSettlement `json:"months"`
	// People is the same ledger rolled up per TA — see PersonSettlement.
	People      []PersonSettlement `json:"people"`
	PaidBaht    float64            `json:"paid_baht"`
	DroppedBaht float64            `json:"dropped_baht"`
	// CutoffMonth/CutoffDate/CutoffStart name the FIRST คาบ that was not funded
	// in full, and nothing more than that. They are a label for the screens and
	// the shortfall notice, never a rule: fundedShare is the only thing that may
	// be asked how much of a given คาบ is paid.
	CutoffMonth string `json:"cutoff_month,omitempty"`
	CutoffDate  string `json:"cutoff_date,omitempty"`
	CutoffStart string `json:"cutoff_start,omitempty"`
	// shareIndex is Slots keyed for lookup — see fundedShare. Unexported and
	// untagged: it is a view of Slots, never a second source of truth, and it
	// must not cross the API boundary where it could drift from them.
	shareIndex map[slotKey]float64
}

// slotKey identifies one person's คาบ the way the ledger and the claim rows both
// spell it: who worked it, on what date, starting when.
type slotKey struct {
	ta          uuid.UUID
	date, start string
}

// SettlementMode is where in the calendar a person's share of a short budget
// lands. Stored on teaching_courses (migration 0105) and chosen by the
// lecturer, who is the one who knows whether the budget can carry the spread.
//
// The mode never changes how much anybody is paid — that is the proportional
// share — only which months carry it.
type SettlementMode string

const (
	// SettleChronological pays months in full in date order until the share
	// runs out. The original and default rule: the tail of the term is paid
	// nothing.
	SettleChronological SettlementMode = "chronological"
	// SettleSpread cuts every month by the same proportion so each one is paid
	// something.
	SettleSpread SettlementMode = "spread"
)

func (m SettlementMode) valid() bool {
	return m == SettleChronological || m == SettleSpread
}

// fundedShare reports what fraction of a person's คาบ at (date, start) the
// budget funds, in [0, 1] — the question every document asks of each cost row
// it is about to sum.
//
// It reads the settled ledger rather than re-deriving the rule. The claim rows
// can carry several cost lines for one คาบ (different end times or levels), so
// callers multiply their own row's baht by this fraction rather than adding a
// slot figure that would then be counted twice.
//
// A คาบ that is not in the ledger at all is funded at 0 — slotLedger drops
// zero-baht costs, which contribute nothing either way, and an unknown key must
// never be assumed paid.
func (t TrackSettlement) fundedShare(ta uuid.UUID, date, start string) float64 {
	return t.shareIndex[slotKey{ta, date, start}]
}

// CourseSettlement answers "what will actually be paid, and what falls off".
type CourseSettlement struct {
	Regular TrackSettlement `json:"regular"`
	Special TrackSettlement `json:"special"`
	// UnpaidMonths is the union across pools, ascending — what the screens
	// name to the lecturer and the TA.
	UnpaidMonths []string `json:"unpaid_months,omitempty"`
	// PartialMonths are paid something but not everything. Named separately
	// because "ได้บางส่วน" and "ไม่ได้เลย" are different sentences to a TA.
	PartialMonths []string `json:"partial_months,omitempty"`
	// TrackUnpaidMonths refines PartialMonths: the months in it where a whole
	// budget pool was paid nothing while another was paid. "ได้บางส่วน" is the
	// truth about the course and a lie to everyone on the empty pool, so the
	// screens need to be able to name the pool.
	TrackUnpaidMonths []MonthTrackShortfall `json:"track_unpaid_months,omitempty"`
	DroppedBaht       float64               `json:"dropped_baht"`
	OverBudget        bool                  `json:"over_budget"`
	// SpilledBaht is capacity the special pool lent to the regular one under the
	// concurrent-section rule. Reported so a course that only balances because
	// of the spill does not look like it simply fitted.
	SpilledBaht float64 `json:"spilled_baht,omitempty"`
}

// MonthTrackShortfall names a month in which one budget pool was paid nothing
// while another was paid something.
//
// Reported apart from the month lists because it is a different sentence to a
// different person: to a TA on ภาคปกติ that month arrived in full, and to a TA
// on ภาคพิเศษ it did not arrive at all. Calling it "ได้บางส่วน" is true of the
// course and false of both of them.
type MonthTrackShortfall struct {
	YearMonth string `json:"year_month"`
	// ZeroTracks are the pools ("regular" / "special") that had work that month
	// and were paid nothing for it.
	ZeroTracks []string `json:"zero_tracks"`
}

// classifyMonths sorts a course's months into what its people need to be told.
//
// Only pools with work that month get a vote: a course with no ภาคพิเศษ section
// must not be reported as having a pool that went unpaid.
//
//   - every pool with work paid nothing  → unpaid   ("ไม่ได้รับเลย")
//   - some pool short, somebody was paid → partial  ("ได้ไม่ครบ")
//   - a whole pool paid nothing, another paid → also partial, and named in
//     tracks so the screen can say WHICH pool got nothing
func classifyMonths(tracks ...TrackSettlement) (unpaid, partial []string, zeroed []MonthTrackShortfall) {
	type state struct {
		withWork, paidNothing, short int
		zeroTracks                   []string
	}
	byMonth := map[string]*state{}
	var order []string
	for _, t := range tracks {
		for _, m := range t.Months {
			if m.Baht <= 0 {
				continue // this pool had no work that month; it has no view
			}
			st, ok := byMonth[m.YearMonth]
			if !ok {
				st = &state{}
				byMonth[m.YearMonth] = st
				order = append(order, m.YearMonth)
			}
			st.withWork++
			switch {
			case m.PaidBaht <= 0.01:
				st.paidNothing++
				st.zeroTracks = append(st.zeroTracks, t.Track)
			case !m.Paid:
				st.short++
			}
		}
	}
	sort.Strings(order)
	for _, ym := range order {
		st := byMonth[ym]
		switch {
		case st.paidNothing == st.withWork:
			unpaid = append(unpaid, ym)
		case st.paidNothing > 0 || st.short > 0:
			partial = append(partial, ym)
		}
		// Named whenever a pool was emptied but the month was not a total loss —
		// the case the month lists alone cannot express.
		if st.paidNothing > 0 && st.paidNothing < st.withWork {
			zeroed = append(zeroed, MonthTrackShortfall{YearMonth: ym, ZeroTracks: st.zeroTracks})
		}
	}
	return unpaid, partial, zeroed
}

// settleTrack decides how much of one pool's work the money reaches.
//
// The pool is shared out BETWEEN PEOPLE in proportion to what each is owed —
// "ใครทำมาก ก็ได้มาก", and two TAs owed the same amount are paid the same
// amount, whatever days they happened to work (the college's ask of
// 07/09/2026: "ต่อให้งบขาด เงินไม่พอ ก็ต้องได้เท่า ๆ กัน ... ถ้าทำงานเวลา
// เท่า ๆ กัน"). The share is money, rounded DOWN to a whole baht (11/09/2026):
// the figure goes on a claim form as a single amount, and a form that says
// ฿1,333 is one finance can transfer. What the rounding leaves — under one baht
// per person — stays in the pool; it is the only money this rule does not
// spend.
//
// A pool that covers everything pays everything as costed, satang included:
// rounding is a consequence of being short, not a haircut on a funded course.
func settleTrack(mode SettlementMode, track string, cap, committed float64, slots []SlotSettlement) TrackSettlement {
	out := TrackSettlement{Track: track, Cap: cap, Committed: committed, Slots: slots}
	// A cap of 0 means "not configured" rather than "no money" — the student
	// count has not been entered yet, and refusing to pay anything on that basis
	// would be a silent zeroing. Treated as unlimited; the export's own
	// student-count gate is what stops a course in that state.
	if cap <= 0 {
		return finishTrack(payInFull(out))
	}

	people, byTA := taOrder(out.Slots)
	owed := map[uuid.UUID]float64{}
	var totalOwed float64
	for _, ta := range people {
		for _, i := range byTA[ta] {
			owed[ta] += out.Slots[i].Baht
		}
		totalOwed += owed[ta]
	}
	pool := cap - committed
	if totalOwed <= pool+0.005 {
		return finishTrack(payInFull(out)) // an exact fit is a fit
	}
	pool = math.Max(0, pool) // the lump alone can exceed the cap; nothing is left then

	for _, ta := range people {
		if owed[ta] <= 0 {
			continue
		}
		share := math.Floor(pool * owed[ta] / totalOwed)
		placeShare(mode, out.Slots, byTA[ta], share)
	}
	return finishTrack(out)
}

// payInFull funds every คาบ at its cost.
func payInFull(out TrackSettlement) TrackSettlement {
	for i := range out.Slots {
		out.Slots[i].PaidBaht = out.Slots[i].Baht
	}
	return out
}

// placeShare lays one person's share over their months under the chosen rule.
// idxs are that person's คาบ in chronological order (slotLedger's order).
func placeShare(mode SettlementMode, slots []SlotSettlement, idxs []int, share float64) {
	months, byMonth := monthOrder(slots, idxs)
	cost := make([]float64, len(months))
	for m, ym := range months {
		for _, i := range byMonth[ym] {
			cost[m] += slots[i].Baht
		}
	}
	var allot []float64
	if mode == SettleSpread {
		allot = spreadOverMonths(share, cost)
	} else {
		allot = fillMonthsInOrder(share, cost)
	}
	for m, ym := range months {
		placeInMonth(slots, byMonth[ym], allot[m], cost[m])
	}
}

// fillMonthsInOrder pays months in full, earliest first, until the share can
// no longer cover the next one; that month takes whatever is left and the
// months after it take nothing. The full months carry their exact cost, so the
// partial month's figure is what keeps the person's total at the whole-baht
// share.
func fillMonthsInOrder(share float64, cost []float64) []float64 {
	allot := make([]float64, len(cost))
	left := share
	for m, c := range cost {
		pay := math.Min(c, left)
		allot[m] = pay
		left -= pay
	}
	return allot
}

// spreadOverMonths cuts every month by the same proportion. Each month's figure
// is rounded down to a whole baht so the per-month documents carry whole
// amounts too, and the baht the rounding strands are handed back to the
// earliest months that still have room — so the person's total is exactly
// their share and no month is paid more than it cost.
func spreadOverMonths(share float64, cost []float64) []float64 {
	var total float64
	for _, c := range cost {
		total += c
	}
	allot := make([]float64, len(cost))
	if total <= 0 {
		return allot
	}
	placed := 0.0
	for m, c := range cost {
		allot[m] = math.Floor(share * c / total)
		placed += allot[m]
	}
	left := share - placed
	for m := range cost {
		if left <= 0 {
			break
		}
		room := cost[m] - allot[m]
		add := math.Min(room, left)
		allot[m] += add
		left -= add
	}
	return allot
}

// placeInMonth spreads a month's allotment over its คาบ in proportion to what
// each costs, so a partly funded month is partly funded everywhere in it.
func placeInMonth(slots []SlotSettlement, idxs []int, allot, cost float64) {
	if cost <= 0 {
		return
	}
	for _, i := range idxs {
		slots[i].PaidBaht = allot * slots[i].Baht / cost
	}
}

// taOrder groups slot indexes by person, people in a stable order and each
// person's คาบ in the chronological order slotLedger already sorted them into.
func taOrder(slots []SlotSettlement) ([]uuid.UUID, map[uuid.UUID][]int) {
	byTA := map[uuid.UUID][]int{}
	var people []uuid.UUID
	for i := range slots {
		ta := slots[i].TA
		if _, seen := byTA[ta]; !seen {
			people = append(people, ta)
		}
		byTA[ta] = append(byTA[ta], i)
	}
	sort.Slice(people, func(i, j int) bool { return people[i].String() < people[j].String() })
	return people, byTA
}

// monthOrder groups slot indexes by month, months in calendar order and each
// month's slots in the chronological order slotLedger already sorted them into.
func monthOrder(slots []SlotSettlement, idxs []int) ([]string, map[string][]int) {
	byMonth := map[string][]int{}
	var months []string
	for _, i := range idxs {
		ym := slots[i].YearMonth
		if _, seen := byMonth[ym]; !seen {
			months = append(months, ym)
		}
		byMonth[ym] = append(byMonth[ym], i)
	}
	sort.Strings(months)
	return months, byMonth
}

// finishTrack totals what the shares placed and derives the views built on it.
// Every figure here is read off Slots, so no rule can report a total its own
// คาบ do not add up to.
func finishTrack(out TrackSettlement) TrackSettlement {
	out.shareIndex = make(map[slotKey]float64, len(out.Slots))
	for i := range out.Slots {
		sl := out.Slots[i]
		out.PaidBaht += sl.PaidBaht
		out.DroppedBaht += sl.Baht - sl.PaidBaht
		// The first shortfall, whichever rule produced it. Under the spread rule
		// this is simply the earliest คาบ — a label, not a boundary.
		if out.CutoffDate == "" && sl.PaidBaht < sl.Baht-0.005 {
			out.CutoffDate, out.CutoffStart, out.CutoffMonth = sl.Date, sl.StartTime, sl.YearMonth
		}
		share := 0.0
		if sl.Baht > 0 {
			share = sl.PaidBaht / sl.Baht
		}
		out.shareIndex[slotKey{sl.TA, sl.Date, sl.StartTime}] = share
	}
	out.PaidBaht = round2(out.PaidBaht)
	out.DroppedBaht = round2(out.DroppedBaht)
	out.Months = rollUpMonths(out.Slots)
	out.People = rollUpPeople(out.Slots)
	return out
}

// rollUpPeople turns the คาบ ledger into one row per TA, each with their own
// months. Names and levels are filled in by the caller that has the database;
// here there are only ids, in a stable order.
func rollUpPeople(slots []SlotSettlement) []PersonSettlement {
	people, byTA := taOrder(slots)
	out := make([]PersonSettlement, 0, len(people))
	for _, ta := range people {
		own := make([]SlotSettlement, 0, len(byTA[ta]))
		for _, i := range byTA[ta] {
			own = append(own, slots[i])
		}
		p := PersonSettlement{TAID: ta, Months: rollUpMonths(own)}
		for _, m := range p.Months {
			p.Baht += m.Baht
			p.PaidBaht += m.PaidBaht
		}
		p.Baht, p.PaidBaht = round2(p.Baht), round2(p.PaidBaht)
		out = append(out, p)
	}
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
		out[i].PaidBaht += sl.PaidBaht
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

// ForecastCourseAs answers the same question under a rule the course has NOT
// been switched to. Nothing is written and nothing else changes — it exists so
// the lecturer being asked to choose can see both answers side by side rather
// than flipping the switch to find out what it does.
func (s *ExportService) ForecastCourseAs(
	ctx context.Context, courseID uuid.UUID, mode SettlementMode,
) (*CourseSettlement, error) {
	return s.settleAs(ctx, courseID, mergedSittingsForecastCTE, mode)
}

// settlementMode reads the rule the lecturer chose for this course.
//
// An unreadable or unrecognised value settles chronologically rather than
// failing: that is the rule every course had before the column existed, and
// the one every document already issued was built on.
func (s *ExportService) settlementMode(ctx context.Context, courseID uuid.UUID) (SettlementMode, error) {
	var mode SettlementMode
	if err := s.pool.QueryRow(ctx,
		`SELECT settlement_mode FROM teaching_courses WHERE id = $1`, courseID).Scan(&mode); err != nil {
		return "", err
	}
	if !mode.valid() {
		return SettleChronological, nil
	}
	return mode, nil
}

// settle answers under the rule the course is actually set to.
func (s *ExportService) settle(ctx context.Context, courseID uuid.UUID, sittingsCTE string) (*CourseSettlement, error) {
	mode, err := s.settlementMode(ctx, courseID)
	if err != nil {
		return nil, err
	}
	return s.settleAs(ctx, courseID, sittingsCTE, mode)
}

func (s *ExportService) settleAs(
	ctx context.Context, courseID uuid.UUID, sittingsCTE string, mode SettlementMode,
) (*CourseSettlement, error) {
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
		// เดิมเป็น `if err == nil` เฉย ๆ ซึ่งกลืน error ทิ้ง แล้วปล่อยให้ cap
		// ค้างที่ 0 — ซึ่ง settleTrack อ่านว่า "ไม่จำกัด" ตรงข้ามกับความจริง
		// ที่ว่าเราแค่อ่านเพดานไม่ได้ ⇒ DB สะดุดครั้งเดียว = จ่ายเกินงบเงียบ ๆ
		// ทั้งคอร์ส "ยังไม่ตั้งค่า" กับ "อ่านไม่ได้" ต้องไม่ลงเอยที่ผลลัพธ์เดียวกัน
		snap, err := s.budget.Compute(ctx, courseID)
		if err != nil {
			return nil, fmt.Errorf("settle %s: อ่านเพดานงบไม่สำเร็จ: %w", courseID, err)
		}
		capRegular, capSpecial = snap.TermPayRegular, snap.TermPaySpecial
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
		Regular: settleTrack(mode, "regular", capRegular+spill, 0, byTrack["regular"]),
		Special: settleTrack(mode, "special", specialLeft, committedSpecial, byTrack["special"]),
	}
	out.SpilledBaht = round2(spill)
	out.DroppedBaht = round2(out.Regular.DroppedBaht + out.Special.DroppedBaht)
	out.OverBudget = out.DroppedBaht > 0

	// The grad-special lump holders have no คาบ and so no row yet; they are the
	// people the Committed figure is made of, listed so the screen can name
	// them beside everybody else instead of showing one unattributed sum.
	if gradSpecialTAs > 0 {
		holders, err := s.gradSpecialTAIDs(ctx, courseID)
		if err != nil {
			return nil, err
		}
		out.Special.CommittedByMonth = map[string]float64{}
		for _, ta := range holders {
			// Dated by the holder's own approved special-track hours (or the
			// forecast's not-yet-rejected ones), never by the settlement mode.
			byMonth, err := s.gradLumpByMonth(ctx, courseID, ta, gradLump,
				sittingsCTE != mergedSittingsForecastCTE)
			if err != nil {
				return nil, err
			}
			p := PersonSettlement{TAID: ta, LumpBaht: gradLump, Months: []MonthSettlement{}}
			for ym, amt := range byMonth {
				p.Months = append(p.Months, MonthSettlement{YearMonth: ym, Baht: amt, PaidBaht: amt, Paid: true})
				out.Special.CommittedByMonth[ym] += amt
			}
			sort.Slice(p.Months, func(i, j int) bool { return p.Months[i].YearMonth < p.Months[j].YearMonth })
			out.Special.People = append(out.Special.People, p)
		}
	}
	if err := s.namePeople(ctx, courseID, out); err != nil {
		return nil, err
	}

	// "ไม่ได้เลย" and "ได้บางส่วน" are separated here rather than at the screen:
	// a month that lost one คาบ and one that lost all of them are different news
	// for the person who worked them, and only the settlement knows which is
	// which.
	//
	// The classification is per MONTH but decided across BOTH pools, and the
	// pools are what makes it subtle. ภาคปกติ and ภาคพิเศษ are separate budgets
	// that run out at different points, so a month can pay one in full and the
	// other nothing.
	//
	// (07/09/2026) That case used to be reported as "ไม่ได้รับค่าตอบแทน" for the
	// whole month. The old loop set a flag when a pool paid nothing and another
	// when a pool was short, but a pool paid IN FULL set neither — so a month
	// that was complete on ภาคปกติ and empty on ภาคพิเศษ looked, to the code
	// below, exactly like a month nobody was paid for. CP363205 announced that
	// nobody would be paid for ตุลาคม while its regular track was being paid
	// ฿2,040 in full. The comment here has always described the intended rule;
	// what was missing was code that carried it out.
	out.UnpaidMonths, out.PartialMonths, out.TrackUnpaidMonths = classifyMonths(out.Regular, out.Special)
	return out, nil
}

// namePeople fills Name and Level on every PersonSettlement of both pools from
// the course's approved assignments. Level is per (TA, track): the assignment
// that put them on that pool. A TA the query no longer returns (dropped after
// logging) keeps an empty name rather than failing the settlement — the money
// figures do not depend on it.
func (s *ExportService) namePeople(ctx context.Context, courseID uuid.UUID, c *CourseSettlement) error {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.ta_id, sec.track::text, a.level::text,
		       COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		       u.first_name || ' ' || u.last_name
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec  ON sec.id = a.section_id AND sec.teaching_course_id = $1
		JOIN users u       ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id`, courseID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type key struct {
		ta    uuid.UUID
		track string
	}
	level := map[key]string{}
	name := map[uuid.UUID]string{}
	for rows.Next() {
		var k key
		var lvl, n string
		if err := rows.Scan(&k.ta, &k.track, &lvl, &n); err != nil {
			return err
		}
		level[k] = lvl
		name[k.ta] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fill := func(t *TrackSettlement) {
		for i := range t.People {
			p := &t.People[i]
			p.Name = name[p.TAID]
			p.Level = level[key{p.TAID, t.Track}]
		}
		sort.SliceStable(t.People, func(i, j int) bool { return t.People[i].Name < t.People[j].Name })
	}
	fill(&c.Regular)
	fill(&c.Special)
	return nil
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

// dropUnpaidWork rewrites each TA's actualPaid to cover only what the budget
// funds of their work.
//
// payBaht stays as earned — the two figures are what the preview compares, and
// the gap between them is precisely the money the shortfall took away.
// Recomputed per TA from their own sittings through fundedShare rather than
// scaled from the record's total, so the payout and the claim document sum the
// same rows the same way.
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
		lost[c.TA] += c.Baht * (1 - t.fundedShare(c.TA, c.Date, c.StartTime))
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
	// Mode is the rule these figures were produced under, and Alternative is the
	// same forecast under the other one. Shipped together so the lecturer decides
	// by comparing two answers rather than by flipping the switch to find out.
	Mode        SettlementMode    `json:"settlement_mode"`
	Alternative *CourseSettlement `json:"alternative_forecast,omitempty"`
	// CanChangeMode answers for THIS viewer: a lecturer loses the switch once
	// staff have checked a month, while staff keep it until the money is sent.
	// LockedMonths names the months that closed it and LockReason says which
	// rule did — the screen needs to explain a disabled button, not just show
	// one.
	CanChangeMode bool     `json:"can_change_mode"`
	LockedMonths  []string `json:"locked_months,omitempty"`
	// LockReason is "finance_sent" (nobody may change it) or "staff_reviewed"
	// (only staff may), empty when the switch is open.
	LockReason string `json:"lock_reason,omitempty"`
	// ReexportMonths are already-exported months whose claim documents would
	// have to be downloaded again if the rule changed now. Shown to staff BEFORE
	// they change it, because re-issuing is work somebody has to do.
	ReexportMonths []string `json:"reexport_months,omitempty"`
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
	mode, err := s.settlementMode(ctx, courseID)
	if err != nil {
		return nil, err
	}
	other := SettleSpread
	if mode == SettleSpread {
		other = SettleChronological
	}
	alternative, err := s.ForecastCourseAs(ctx, courseID, other)
	if err != nil {
		return nil, err
	}
	sent, err := s.courseMonthsAtStatus(ctx, courseID, []string{"finance_sent"})
	if err != nil {
		return nil, err
	}
	reviewed, err := s.courseMonthsAtStatus(ctx, courseID, []string{"staff_reviewed", "exported"})
	if err != nil {
		return nil, err
	}
	exported, err := s.courseMonthsAtStatus(ctx, courseID, []string{"exported"})
	if err != nil {
		return nil, err
	}
	view := &SettlementView{
		Committed:      committed,
		Forecast:       forecast,
		Mode:           mode,
		Alternative:    alternative,
		CanChangeMode:  true,
		ReexportMonths: exported,
	}
	switch {
	case len(sent) > 0:
		view.CanChangeMode, view.LockReason, view.LockedMonths = false, "finance_sent", sent
	case !privileged && len(reviewed) > 0:
		view.CanChangeMode, view.LockReason, view.LockedMonths = false, "staff_reviewed", reviewed
	}
	return view, nil
}

// courseMonthsAtStatus names the course's months sitting at any of the given
// submission-period statuses, for any TA on it.
//
// Course-wide rather than per-assignment (financeLockedMonths' question) because
// the settlement rule is a property of the whole course: changing it re-decides
// which คาบ get paid in every month at once.
func (s *ExportService) courseMonthsAtStatus(ctx context.Context, courseID uuid.UUID, statuses []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT sp.label
		FROM submission_period_status st
		JOIN submission_periods sp ON sp.id = st.submission_period_id
		WHERE st.teaching_course_id = $1 AND st.status = ANY($2)
		ORDER BY sp.label`, courseID, statuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetSettlementMode changes how this course's budget is cut when it falls short.
//
// Allowed for the course's own lecturers and for staff, in both directions. The
// lecturer is the one who knows whether the budget can carry the spread, and
// staff are who they ask when they cannot reach the screen themselves; a switch
// that only turns on would leave a lecturer who changed their mind with no way
// back and a support request nobody can action.
//
// Refused outright once ANY month of the course has been exported or sent to
// finance — not "applied only to the months still open". The rule divides one
// pool across the whole term, so re-running it after part of that pool has
// already been paid out would produce figures that do not reconcile with the
// documents finance is holding.
func (s *ExportService) SetSettlementMode(
	ctx context.Context, actor, courseID uuid.UUID, mode SettlementMode, privileged bool,
) error {
	if !mode.valid() {
		return Invalid("รูปแบบการแบ่งงบไม่ถูกต้อง")
	}
	if !privileged {
		var teaches bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM teaching_lecturers
			                WHERE teaching_course_id = $1 AND lecturer_id = $2)`,
			courseID, actor).Scan(&teaches); err != nil {
			return err
		}
		if !teaches {
			return ErrForbidden
		}
	}

	sent, err := s.courseMonthsAtStatus(ctx, courseID, []string{"finance_sent"})
	if err != nil {
		return err
	}
	if len(sent) > 0 {
		// Past re-issuing a document: the payout itself has gone. Reversing that
		// is the admin unlock, not a settings change.
		return Conflict(fmt.Sprintf(
			"เปลี่ยนวิธีแบ่งงบไม่ได้ เดือน %s ส่งการเงินไปแล้ว "+
				"หากจำเป็นต้องแก้ กรุณาให้ผู้ดูแลระบบปลดล็อกก่อน",
			strings.Join(sent, ", ")))
	}

	reviewed, err := s.courseMonthsAtStatus(ctx, courseID, []string{"staff_reviewed", "exported"})
	if err != nil {
		return err
	}
	if !privileged && len(reviewed) > 0 {
		// The lecturer's window closes when staff sign the month off. After that
		// the figures are staff's to answer for, and a lecturer moving them
		// underneath a checked document would leave the officer defending
		// numbers they never saw.
		return Conflict(fmt.Sprintf(
			"เปลี่ยนวิธีแบ่งงบไม่ได้ เจ้าหน้าที่ตรวจสอบเดือน %s แล้ว "+
				"หากต้องการเปลี่ยน กรุณาติดต่อเจ้าหน้าที่",
			strings.Join(reviewed, ", ")))
	}

	// Read before write rather than RETURNING a subquery: whether a subquery in
	// RETURNING sees the old row or the new one is exactly the kind of thing to
	// not be clever about when the answer decides who gets told their pay moved.
	previous, err := s.settlementMode(ctx, courseID)
	if err != nil {
		return err
	}
	if previous == mode {
		return nil // already there; nothing happened, so nobody is told anything
	}

	// Everything below is one transaction: a mode that changed while the
	// documents built on the old one stayed valid is the one state this must
	// never be left in.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		UPDATE teaching_courses
		   SET settlement_mode = $2, settlement_mode_by = $3, settlement_mode_at = NOW()
		 WHERE id = $1`, courseID, string(mode), actor); err != nil {
		return err
	}

	// Any month already exported was exported under the OLD split, so its claim
	// document no longer says what the system says. Send those months back to
	// staff_reviewed and drop the course's export flag: the file has to be
	// downloaded again before it can go to finance, and the officer cannot miss
	// that it needs re-issuing.
	reissue, err := s.reopenExportedMonths(ctx, tx, courseID)
	if err != nil {
		return err
	}

	note := string(previous) + " → " + string(mode)
	if len(reissue) > 0 {
		note += " (ต้องออกใบเบิกใหม่: " + strings.Join(reissue, ", ") + ")"
	}
	// The previous mode was already known here and was only ever written into
	// the note as free text. In the columns it is queryable: "every course whose
	// split was changed away from เฉลี่ย after the term started" is a question
	// a note cannot answer.
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actor, Action: "course.settlement_mode",
		Entity: "teaching_course", EntityID: courseID.String(),
		Note:   note,
		Before: map[string]any{"settlement_mode": string(previous)},
		After: map[string]any{
			"settlement_mode": string(mode),
			// Naming the months forced back to staff_reviewed matters: this is
			// the trail's only record that a claim document already in
			// circulation stopped matching the system.
			"reissue_months": reissue,
		},
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// The people whose pay just moved hear about it. Which months they are paid
	// for has changed, and they cannot see the switch that changed it.
	s.notifySettlementModeChanged(ctx, courseID, mode)
	return nil
}

// reopenExportedMonths undoes the export lock on every month of a course, so the
// claim documents must be produced again. Returns the months it reopened.
//
// Called only when the settlement rule changes: the figures on an issued
// document were computed under the previous rule, and leaving them locked would
// let a file that no longer matches the system reach the finance office.
func (s *ExportService) reopenExportedMonths(ctx context.Context, tx pgx.Tx, courseID uuid.UUID) ([]string, error) {
	rows, err := tx.Query(ctx, `
		UPDATE submission_period_status st
		   SET status = 'staff_reviewed',
		       exported_at = NULL, exported_by = NULL, exported_name = NULL
		  FROM submission_periods sp
		 WHERE sp.id = st.submission_period_id
		   AND st.teaching_course_id = $1
		   AND st.status = 'exported'
		RETURNING sp.label`, courseID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			rows.Close()
			return nil, err
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	// The course-level one-shot flag too, or the export screen still reads the
	// course as done and never offers the download again.
	if _, err := tx.Exec(ctx,
		`UPDATE teaching_courses SET exported_at = NULL WHERE id = $1`, courseID); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func (s *ExportService) notifySettlementModeChanged(ctx context.Context, courseID uuid.UUID, mode SettlementMode) {
	if s.notify == nil {
		return
	}
	var code string
	if err := s.pool.QueryRow(ctx,
		`SELECT code FROM teaching_courses WHERE id = $1`, courseID).Scan(&code); err != nil {
		return
	}
	link := "/ta/courses/" + courseID.String() + "/worklog"
	body := "อาจารย์เปลี่ยนวิธีแบ่งงบเป็น “เฉลี่ยให้ได้ครบทุกเดือน” " +
		"ทุกเดือนจะถูกหักเป็นสัดส่วนเท่ากัน ยอดรวมที่ได้รับไม่เปลี่ยน"
	if mode == SettleChronological {
		body = "อาจารย์เปลี่ยนวิธีแบ่งงบกลับเป็นแบบเดิม (จ่ายเรียงเดือนจนงบหมด) " +
			"เดือนต้นเทอมจะได้เต็ม ส่วนเดือนท้ายอาจไม่ได้รับ ยอดรวมที่ได้รับไม่เปลี่ยน"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.ta_id
		FROM ta_request_assignments a
		JOIN sections sec ON sec.id = a.section_id
		WHERE sec.teaching_course_id = $1 AND a.state <> 'dropped'`, courseID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ta uuid.UUID
		if err := rows.Scan(&ta); err != nil {
			log.Printf("notifySettlementModeChanged scan %s: %v", courseID, err)
			return
		}
		s.notify.Send(ctx, ta, "วิธีแบ่งงบของ "+code+" เปลี่ยนแปลงแล้ว", body, link)
	}
	if err := rows.Err(); err != nil {
		log.Printf("notifySettlementModeChanged rows %s: %v", courseID, err)
	}
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
	// Two different sentences: a month paid part of its worth still pays
	// something, and saying "จะไม่ได้รับค่าตอบแทน" about it would be wrong.
	var what []string
	if len(forecast.PartialMonths) > 0 {
		what = append(what, "เดือน "+strings.Join(thaiMonthLabels(forecast.PartialMonths), ", ")+" ได้ไม่เต็มจำนวน")
	}
	if len(forecast.UnpaidMonths) > 0 {
		what = append(what, "เดือน "+strings.Join(thaiMonthLabels(forecast.UnpaidMonths), ", ")+" ไม่ได้รับค่าตอบแทน")
	}
	body := fmt.Sprintf(
		"%s %s\nงบรายวิชาไม่พอจ่ายทั้งหมด %s (ขาดรวม %.0f บาท "+
			"TA ทุกคนถูกหักเป็นสัดส่วนเท่ากันตามค่าตอบแทนที่ควรได้)\n"+
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
			log.Printf("NotifyBudgetShortfall scan %s: %v", courseID, err)
			return
		}
		s.notify.Send(ctx, ta, title, body,
			"/ta/courses/"+courseID.String()+"/worklog")
	}
	if err := rows.Err(); err != nil {
		log.Printf("NotifyBudgetShortfall rows %s: %v", courseID, err)
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
	return b2OverlapScopedCTE(statusFilter, "s1.teaching_course_id = $1 AND s2.teaching_course_id = $1")
}

// b2OverlapTermCTE is the same chain over every course of term $1 — for the
// staff review queue, which is read a term at a time. b2_blocks and b2_overlap
// carry teaching_course_id so a TA on two courses is not paired across them.
func b2OverlapTermCTE(statusFilter string) string {
	return b2OverlapScopedCTE(statusFilter,
		"s1.teaching_course_id = s2.teaching_course_id AND s1.teaching_course_id IN (SELECT id FROM teaching_courses WHERE term_id = $1)")
}

func b2OverlapScopedCTE(statusFilter, scope string) string {
	return `
	b2_pairs AS (
	    SELECT a1.ta_id, s1.teaching_course_id, w1.work_date,
	           GREATEST(w1.start_time, w2.start_time) AS starts_at,
	           LEAST(w1.end_time, w2.end_time)        AS ends_at
	    FROM work_logs w1
	    JOIN ta_request_assignments a1 ON a1.id = w1.assignment_id AND a1.level::text = 'undergrad'
	    JOIN sections s1 ON s1.id = a1.section_id AND s1.track = 'regular'
	    JOIN ta_request_assignments a2 ON a2.ta_id = a1.ta_id
	    JOIN sections s2 ON s2.id = a2.section_id AND s2.track = 'special'
	    JOIN work_logs w2 ON w2.assignment_id = a2.id AND w2.work_date = w1.work_date
	    WHERE ` + scope + `
	      AND ` + statusFilter + `
	      AND w1.start_time < w2.end_time AND w2.start_time < w1.end_time
	),
	b2_marked AS (
	    SELECT *,
	           CASE WHEN starts_at <= MAX(ends_at) OVER (
	                    PARTITION BY ta_id, teaching_course_id, work_date
	                    ORDER BY starts_at, ends_at
	                    ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING)
	                THEN 0 ELSE 1 END AS starts_new
	    FROM b2_pairs
	),
	b2_grouped AS (
	    SELECT *,
	           SUM(starts_new) OVER (
	               PARTITION BY ta_id, teaching_course_id, work_date
	               ORDER BY starts_at, ends_at
	               ROWS UNBOUNDED PRECEDING) AS block
	    FROM b2_marked
	),
	b2_blocks AS (
	    SELECT ta_id, teaching_course_id, work_date,
	           MIN(starts_at) AS starts_at, MAX(ends_at) AS ends_at
	    FROM b2_grouped GROUP BY ta_id, teaching_course_id, work_date, block
	),
	b2_overlap AS (
	    SELECT ta_id, teaching_course_id, to_char(work_date, 'YYYY-MM') AS ym,
	           SUM(EXTRACT(EPOCH FROM (ends_at - starts_at)) / 3600.0) AS hours
	    FROM b2_blocks GROUP BY 1, 2, 3
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
	    SELECT ta_id, work_date, start_time, end_time,
	           to_char(work_date, 'YYYY-MM-DD') AS d,
	           to_char(start_time, 'HH24:MI')   AS st,
	           to_char(end_time, 'HH24:MI')     AS et,
	           to_char(work_date, 'YYYY-MM')    AS ym,
	           track, level, SUM(hours) AS hrs
	    FROM sittings
	    WHERE teaching_course_id = $1
	    GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10
	),
	-- Raw (uncapped, B2-adjusted) value per คาบ. The B2 clock time is clipped
	-- out of the special คาบ it actually falls in — the same cut the printed
	-- special sheet makes (clipSpecialOverlap) — so a lecture taught to both
	-- tracks at once is worth 0 on the special side and the คาบ around it keep
	-- their whole value. Smearing the month's overlap across every special คาบ
	-- pro-rata gave the same month total but fractional คาบ (฿199.99 months)
	-- and a per-คาบ split the claim sheet could not reproduce.
	priced AS (
	    SELECT sit.*,
	           CASE
	               WHEN sit.level = 'undergrad' AND sit.track = 'regular' THEN sit.hrs * $2
	               WHEN sit.level IN ('master','phd') AND sit.track = 'regular' THEN sit.hrs * $4
	               WHEN sit.level = 'undergrad' AND sit.track = 'special' THEN
	                   GREATEST(sit.hrs - COALESCE(ov.hours, 0), 0) * $3
	               ELSE 0
	           END AS raw_baht
	    FROM sit
	    LEFT JOIN LATERAL (
	        SELECT SUM(EXTRACT(EPOCH FROM (
	                   LEAST(b.ends_at, sit.end_time) - GREATEST(b.starts_at, sit.start_time)
	               )) / 3600.0) AS hours
	        FROM b2_blocks b
	        WHERE sit.track = 'special'
	          AND b.ta_id = sit.ta_id AND b.work_date = sit.work_date
	          AND b.starts_at < sit.end_time AND b.ends_at > sit.start_time
	    ) ov ON TRUE
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
	type key struct {
		ta    uuid.UUID
		d, st string
	}
	idx := map[key]int{}
	var out []SlotSettlement
	for _, c := range costs {
		if c.Track != track || c.Baht <= 0 {
			continue
		}
		k := key{c.TA, c.Date, c.StartTime}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, SlotSettlement{
				TA: c.TA, Date: c.Date, StartTime: c.StartTime, YearMonth: c.YearMonth})
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
