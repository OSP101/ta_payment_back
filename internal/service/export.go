package service

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/storage"
)

type ExportService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	store  storage.Store
	budget *BudgetService
	// notify carries the budget-shortfall warning to the lecturer and the TAs.
	notify *NotifyService
	// teaching renders the weekly timetable form. Held as a dependency rather
	// than duplicated here so the zip ships the same document the TA prints.
	teaching *TeachingService
	// users answers TASeniority for the transfer-cover document (Phase 3).
	users *UserService
	// docs holds RevealCitizenID — the only decrypt path for the transfer-cover
	// document's พร้อมเพย์ column (Phase 3).
	docs *DocsService
}

// exportRow is one TA's aggregated numbers used by both the coversheet and
// the PDF renderer. actualPaid ≤ payBaht when the course is over budget and
// pro-rata scaling was applied. isReturning is the "เก่า/ใหม่" badge.
type exportRow struct {
	taID        uuid.UUID
	fullName    string
	email       string
	track       string
	level       string
	hoursTotal  float64
	payRegular  float64 // earned from regular-track work (pre-cap)
	paySpecial  float64 // earned from special-track work incl. grad lump (pre-cap)
	payBaht     float64
	actualPaid  float64
	isReturning bool
}

// exportComputation is the fully-priced result of a course export, shared by
// the ZIP builder and the read-only preview so both show identical numbers.
type exportComputation struct {
	courseCode string
	courseName string
	termMonths int
	records    []exportRow
	budgetMax  float64
	prorated   bool
	settlement *CourseSettlement
}

// ExportPreviewRow is one TA's payout line for the staff preview (read-only).
// It mirrors exportRow but with display-ready labels + a per-TA readiness flag
// so staff can spot and fix problems BEFORE the locking download.
type ExportPreviewRow struct {
	TAID        string  `json:"ta_id"`
	FullName    string  `json:"full_name"`
	Email       string  `json:"email"`
	Level       string  `json:"level"`    // raw: undergrad/master/phd
	LevelTH     string  `json:"level_th"` // ปริญญาตรี/โท/เอก
	Track       string  `json:"track"`    // raw: regular/special
	TrackTH     string  `json:"track_th"` // ภาคปกติ/ภาคพิเศษ
	HoursTotal  float64 `json:"hours_total"`
	PayBaht     float64 `json:"pay_baht"`    // earned before budget cap
	ActualPaid  float64 `json:"actual_paid"` // after pro-rata cap
	IsReturning bool    `json:"is_returning"`
	// The national ID and bank account are deliberately absent: they are not
	// stored (migration 0047) and the finance package carries them inside the
	// creditor-form PDF instead.
	ProfileReady bool   `json:"profile_ready"`
	ProfileIssue string `json:"profile_issue"` // "" when ready
}

// ExportPreview is the JSON payload for GET /exports/course/:id/preview.
type ExportPreview struct {
	TeachingCourseID string  `json:"teaching_course_id"`
	CourseCode       string  `json:"course_code"`
	CourseNameTH     string  `json:"course_name_th"`
	TermMonths       int     `json:"term_months"`
	BudgetMax        float64 `json:"budget_max"`
	TotalPay         float64 `json:"total_pay"`    // Σ pay_baht
	TotalActual      float64 `json:"total_actual"` // Σ actual_paid
	OverBudget       bool    `json:"over_budget"`
	Prorated         bool    `json:"prorated"`
	// Settlement names the months the budget could not reach. Sent to every
	// screen that has to warn somebody, so the lecturer and the TA see the same
	// months the export will actually drop.
	Settlement *CourseSettlement `json:"settlement,omitempty"`
	// AllReady covers the TA PROFILES (approved profile + creditor form).
	AllReady bool `json:"all_ready"`
	// Blockers is every stage of the pipeline still unfinished — the same list
	// BuildCourseZip refuses on. CanExport is the single answer the download
	// button reads; deriving it on the client is how the button came to offer
	// what the server refuses.
	Blockers  []ExportBlocker    `json:"blockers"`
	CanExport bool               `json:"can_export"`
	Rows      []ExportPreviewRow `json:"rows"`
}

// round2 rounds a baht amount to 2 decimals — every figure that lands on a
// payroll document goes through this so float64 accumulation noise (e.g.
// 1234.5600000001) never reaches the spreadsheet.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// isReturningTA reports whether the TA had an approved assignment in any prior
// academic term. Used by exports to badge each row as "เก่า/ใหม่". Q&A rule:
// "old" = held any approved ta_request_assignment in a term whose start_date
// is earlier than the current course's term.
func (s *ExportService) isReturningTA(ctx context.Context, taID, currentTermID uuid.UUID) bool {
	var yes bool
	_ = s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM ta_request_assignments a
			JOIN ta_requests r ON r.id=a.request_id AND r.status='approved'
			JOIN sections sec ON sec.id=a.section_id
			JOIN teaching_courses tc ON tc.id=sec.teaching_course_id
			JOIN academic_terms t  ON t.id=tc.term_id
			JOIN academic_terms cur ON cur.id=$2
			WHERE a.ta_id=$1 AND (t.academic_year < cur.academic_year
			     OR (t.academic_year = cur.academic_year AND t.semester < cur.semester))
		)`, taID, currentTermID).Scan(&yes)
	return yes
}

// BuildCourseZip builds a zip archive containing per-TA .xlsx files for a course.
// Each xlsx has three sheets:
//
//	Sheet 1: "หน้าปะ" (coversheet) — reimbursement summary
//	Sheet 2: "บันทึกเวลา" (work log)
//	Sheet 3: "ตารางสอน+งาน" (schedule)
//
// Payment model (ประกาศ 731/2565 + 1080/2565 + Q&A 2026):
//
//	ป.ตรี ปกติ  → 40 ฿ × approved hours
//	ป.ตรี พิเศษ → 50 ฿ × approved hours
//	บัณฑิต ปกติ → 50 ฿ × approved hours (hourly per Q&A rule 6c, was lump-sum)
//	บัณฑิต พิเศษ → min(4,000 × term_months, 12,000) — flat monthly capped per term
//
// Billing uses two independent budget pools (ภาคปกติ / ภาคพิเศษ), each settled
// separately in budget_settlement.go. Concurrent-section rule (B2): when an
// undergrad TA logs regular AND special work at the SAME clock time, the
// overlapping hours are counted ONCE at the regular rate ("เบิกภาคปกติก่อน") —
// the duplicated hours are removed from the special side after aggregation.
// Grad tiers are unaffected (grad-special is a flat lump).
// buildExportRows performs the full payout computation for a course (pay rates,
// overlap dedup, grad tiers, two-pool pro-rata budget cap) and returns the
// priced rows without any side effects or readiness gate. Both the ZIP builder
// and the read-only preview call this so their numbers can never drift.
// months (Gregorian "YYYY-MM", empty = the whole term) restricts the claim to
// one fiscal slice — the งบแผ่นดิน year closes 30 กันยายน mid-ภาคต้น, so มิ.ย.–ก.ย.
// and ตุลาคม are claimed on separate documents against separate appropriations.
//
// The SETTLEMENT is deliberately not sliced: the budget stays one pool per
// course for the whole term, cut chronologically as always, and a slice only
// filters that one result. Every slice of a term therefore sums back to the
// undivided figure — no คาบ billed twice, none lost between two documents.
func (s *ExportService) buildExportRows(ctx context.Context, teachingCourseID uuid.UUID, months []string) (*exportComputation, error) {
	var courseCode, courseName string
	if err := s.pool.QueryRow(ctx, `
		SELECT tc.code, tc.name_th FROM teaching_courses tc
		WHERE tc.id=$1`, teachingCourseID).Scan(&courseCode, &courseName); err != nil {
		return nil, err
	}

	var pr PayRate
	_ = s.pool.QueryRow(ctx,
		`SELECT undergrad_regular, undergrad_special, graduate_regular, graduate_special_lumpsum,
		        graduate_regular_hourly, grad_special_term_cap, ug_special_monthly_cap, term_months
		 FROM pay_rates ORDER BY effective_from DESC LIMIT 1`).Scan(
		&pr.UndergradRegular, &pr.UndergradSpecial, &pr.GraduateRegular, &pr.GraduateSpecialLumpsum,
		&pr.GraduateRegularHourly, &pr.GradSpecialTermCap, &pr.UGSpecialMonthlyCap, &pr.TermMonths)
	// Prefer per-term months (source of truth); fall back to pay_rates.term_months.
	var perTermMonths int
	_ = s.pool.QueryRow(ctx, `
		SELECT t.months FROM academic_terms t
		JOIN teaching_courses tc ON tc.term_id = t.id WHERE tc.id = $1`, teachingCourseID).Scan(&perTermMonths)
	if perTermMonths > 0 {
		pr.TermMonths = perTermMonths
	}
	termMonths := pr.TermMonths
	if termMonths == 0 {
		termMonths = 4
	}

	// Billable hours per (TA, track), counted as SITTINGS rather than as raw
	// work_logs rows — two sections taught in one sitting are paid once. This is
	// what the signed claim form has always billed; see billable_hours.go.
	//
	// Loaded separately because a sitting spans sections and cannot be split back
	// across the per-assignment rows below: the whole (TA, track) total is
	// attributed to the FIRST assignment of that pair and the rest are zeroed,
	// which leaves the per-TA aggregation underneath exactly as it was.
	billable, err := s.billableHoursByTATrack(ctx, teachingCourseID, months)
	if err != nil {
		return nil, err
	}
	claimed := map[taTrackKey]bool{}

	// Which of the term's months this document covers, for apportioning the
	// figures that are flat per TERM and have no คาบ to filter (the
	// graduate-special lump). 1 when unscoped.
	monthShare, inSlice, err := s.courseMonthShare(ctx, teachingCourseID, months)
	if err != nil {
		return nil, err
	}

	// Pull per-assignment rows; each is billed at its own track's rate into the
	// regular or special pool (two-pool cap applied after aggregation).
	// Names carry the คำนำหน้า: the timetable files in the export ZIP are
	// named after the TA the way the office files them
	// ("ตารางเรียน-นายสุพพิธาน ภักสวัสดิ์.xlsx"), prefix included.
	assignRows, err := s.pool.Query(ctx, `
		SELECT a.id, a.ta_id,
		       COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		       u.first_name||' '||u.last_name, u.email,
		       sec.track::text, a.level::text,
		       COALESCE(SUM(wl.hours) FILTER (WHERE wl.status='approved'), 0) AS approved_hrs,
		       (SELECT COALESCE(SUM(EXTRACT(EPOCH FROM (end_time-start_time))/3600),0)
		        FROM section_schedules WHERE section_id=sec.id) AS sched_hrs_per_week
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status='approved'
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		JOIN sections sec ON sec.id = a.section_id
		LEFT JOIN work_logs wl ON wl.assignment_id = a.id
		WHERE r.teaching_course_id = $1
		GROUP BY a.id, a.ta_id, tp.prefix, u.title, u.first_name, u.last_name, u.email,
		         sec.track, sec.id, a.level, sec.sec_no
		ORDER BY u.first_name, a.ta_id, (sec.track='regular') DESC, sec.sec_no`, teachingCourseID)
	if err != nil {
		return nil, err
	}
	defer assignRows.Close()

	// Per-TA accumulator so we can (a) apply regular-first spillover for
	// undergrad and (b) aggregate all of a TA's assignments into ONE workbook row
	// (matches the pre-refactor UX where each TA yields a single .xlsx).
	type taAgg struct {
		taID     uuid.UUID
		fullName string
		email    string
		// display: "regular" if TA has any regular assignment else "special";
		// display: "undergrad" if any undergrad assignment else "master"/"phd".
		track      string
		level      string
		hoursTotal float64
		// Pay is split by track so the course budget can be consumed as TWO
		// separate pools (ภาคปกติ then ภาคพิเศษ) — the regular pool funds
		// regular-track work, the special pool funds special-track work. No
		// cross-track rate spillover (superseded by the two-pool rule).
		payRegular     float64
		paySpecial     float64
		hasGradSpecial bool // TA holds ≥1 grad-special section in this course
	}
	byTA := map[uuid.UUID]*taAgg{}
	order := []uuid.UUID{}

	for assignRows.Next() {
		var (
			assignID        uuid.UUID
			taID            uuid.UUID
			fullName, email string
			track, level    string
			approvedHrs     float64
			schedHrsPerWeek float64
		)
		if err := assignRows.Scan(&assignID, &taID, &fullName, &email,
			&track, &level, &approvedHrs, &schedHrsPerWeek); err != nil {
			return nil, err
		}
		// Replace the raw per-assignment sum with this (TA, track)'s sitting
		// total, once. Rows are ordered by TA then track, so "once" is the first
		// row of each pair.
		key := taTrackKey{taID, track}
		if claimed[key] {
			approvedHrs = 0
		} else {
			approvedHrs = billable[key]
			claimed[key] = true
		}
		agg, ok := byTA[taID]
		if !ok {
			agg = &taAgg{
				taID: taID, fullName: fullName, email: email,
				track: track, level: level,
			}
			byTA[taID] = agg
			order = append(order, taID)
		}
		// Display-track precedence: "regular" over "special" so the coversheet
		// shows the more informative label when a TA has both.
		if track == "regular" {
			agg.track = "regular"
		}
		// Display-level precedence: undergrad wins over grad only if this TA
		// happens to hold both (unusual — study_level is fixed per user).
		if level == "undergrad" {
			agg.level = "undergrad"
		}
		agg.hoursTotal += approvedHrs

		// Hourly pay is priced AFTER the loop, for everyone at once, by
		// claimCostByTASlot — the same source the settlement reads. Here only
		// the grad-special flag is noted (Q&A rule 6b: บัณฑิต special is a flat
		// lump, not hourly, added once per TA below).
		if (level == "master" || level == "phd") && track == "special" {
			agg.hasGradSpecial = true
		}
	}

	// One pricing source for every hourly baht (merged sittings, B2 overlap off
	// the special side, monthly cap) — shared with SettleCourse and
	// dropUnpaidMonths so the pay column, the settlement, and the cut months
	// can never disagree about what an hour costs.
	costs, err := s.claimCostByTASlot(ctx, teachingCourseID, pr, mergedSittingsCTE)
	if err != nil {
		return nil, err
	}
	for _, c := range costs {
		if !inSlice(c.YearMonth) {
			continue
		}
		agg, ok := byTA[c.TA]
		if !ok {
			continue
		}
		if c.Track == "regular" {
			agg.payRegular += c.Baht
		} else {
			agg.paySpecial += c.Baht
		}
	}

	// Grad-special lump sum: add once per TA (not per section). Eligibility is
	// just holding an approved grad-special assignment in this course — these
	// TAs no longer log work_logs at all (2026 meeting: the system computes
	// their pay automatically), so hoursTotal is expected to be 0 for them and
	// must NOT gate whether they get paid.
	// graduate_special_lumpsum IS the whole-term-per-course figure — no month
	// multiplication.
	gradLump := pr.GraduateSpecialLumpsum
	if pr.GradSpecialTermCap > 0 && gradLump > pr.GradSpecialTermCap {
		gradLump = pr.GradSpecialTermCap
	}
	// Flat per term, so a month filter cannot select it — apportioned by this
	// course's own regular-track class-schedule share of the selected months
	// (falls back to an even per-month share if no schedule exists yet), which
	// keeps the slices summing to the undivided lump.
	gradShare := monthShare
	if weights, werr := gradSpecialMonthShares(ctx, s.pool, teachingCourseID); werr != nil {
		return nil, werr
	} else if weights != nil {
		gradShare = 0
		if len(months) == 0 {
			gradShare = 1
		} else {
			for _, ym := range months {
				gradShare += weights[ym]
			}
		}
	}
	gradLump *= gradShare
	for _, taID := range order {
		agg := byTA[taID]
		if agg.hasGradSpecial {
			agg.paySpecial += gradLump
		}
	}

	// Look up the current term for old/new detection.
	var currentTermID uuid.UUID
	_ = s.pool.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id=$1`, teachingCourseID).Scan(&currentTermID)

	// Freeze to the shape the workbook + PDF builders share.
	var records []exportRow
	for _, taID := range order {
		agg := byTA[taID]
		total := agg.payRegular + agg.paySpecial
		records = append(records, exportRow{
			taID: agg.taID, fullName: agg.fullName, email: agg.email,
			track: agg.track, level: agg.level,
			hoursTotal: agg.hoursTotal,
			payRegular: agg.payRegular,
			paySpecial: agg.paySpecial,
			// Round once at the freeze point so both the under-budget path and
			// the pro-rata path emit clean 2-decimal figures.
			payBaht:     round2(total),
			actualPaid:  round2(total), // scaled below if a pool is over budget
			isReturning: currentTermID != uuid.Nil && s.isReturningTA(ctx, agg.taID, currentTermID),
		})
	}

	// Two-pool budget cap: regular-track pay is capped at the regular budget
	// (TermPayRegular) and special-track pay at the special budget
	// (TermPaySpecial), independently. This is the "หักงบภาคปกติจนหมดก่อน แล้ว
	// ค่อยภาคพิเศษ" rule — each track draws only from its own pool. budgetMax
	// stays the combined figure for display.
	var budgetMax float64
	if s.budget != nil {
		if snap, err := s.budget.Compute(ctx, teachingCourseID); err == nil {
			budgetMax = snap.PerCourseMaxBaht
		}
	}
	// Month cutoff replaces pro-rata (04/08/2026). Instead of scaling everyone
	// down by a factor nobody can derive from the claim form, whole months are
	// paid in order until the pool runs out and the rest are paid nothing —
	// see budget_settlement.go. Two TAs with the same hours are paid the same.
	settlement, serr := s.SettleCourse(ctx, teachingCourseID)
	if serr != nil {
		return nil, serr
	}
	if settlement.OverBudget {
		// Same month scope as the money that went in: subtracting October's
		// dropped คาบ from a มิ.ย.–ก.ย. document would understate a slice that
		// never contained them.
		if err := s.dropUnpaidWork(ctx, teachingCourseID, records, settlement, pr, months); err != nil {
			return nil, err
		}
	}
	return &exportComputation{
		courseCode: courseCode, courseName: courseName,
		termMonths: termMonths, records: records,
		budgetMax: budgetMax, prorated: settlement.OverBudget,
		settlement: settlement,
	}, nil
}

// BuildCourseZip builds the per-TA .xlsx (+ best-effort .pdf) ZIP for a course.
// It gates on payout readiness, then reuses buildExportRows so the file numbers
// match the preview exactly. Returns (zip bytes, filename, TA count, error).
func (s *ExportService) BuildCourseZip(ctx context.Context, teachingCourseID uuid.UUID, months []string) ([]byte, string, int, error) {
	// Student-count gate: the per-course budget is derived from the enrolled
	// student count (budget.go). If staff never filled it in, num_students is 0,
	// the budget cap is 0, and everyone would be pro-rata'd down to ฿0 silently.
	// Refuse the export with a clear pointer to where to fix it.
	var numStudents int
	if err := s.pool.QueryRow(ctx,
		`SELECT num_students FROM teaching_courses WHERE id=$1`, teachingCourseID).Scan(&numStudents); err != nil {
		return nil, "", 0, err
	}
	if numStudents <= 0 {
		return nil, "", 0, Invalid("ยังไม่ได้กรอกจำนวนนักศึกษาของวิชานี้ กรุณากรอกที่หน้า “วิชาที่เปิดสอน” ก่อนส่งออก (งบเบิกจ่ายคำนวณจากจำนวนนักศึกษา)")
	}
	// Payout readiness gate: refuse to build reimbursement documents while any
	// TA in the course has an unapproved or incomplete profile — otherwise the
	// coversheets ship with blank national-ID/bank fields and nobody notices
	// until the finance office bounces the batch.
	if err := s.validatePayoutReadiness(ctx, teachingCourseID); err != nil {
		return nil, "", 0, err
	}
	// Pipeline gate: every stage — TA sent, lecturer approved, staff signed off
	// — must be finished for every month before any of it becomes a claim
	// document. This is checked on the way OUT, not just in the UI: downloading
	// is the freeze point, and a half-finished download freezes the wrong
	// numbers (see CourseExportBlockers).
	blockers, err := s.CourseExportBlockers(ctx, teachingCourseID, months)
	if err != nil {
		return nil, "", 0, err
	}
	if len(blockers) > 0 {
		return nil, "", 0, exportBlockedError(blockers)
	}
	// Needed by the timetable form, which is scoped to a TERM rather than a
	// course. A missing term is not fatal — the form is simply skipped.
	var termID uuid.UUID
	var academicYear, semester int
	_ = s.pool.QueryRow(ctx, `
		SELECT tc.term_id, t.academic_year, t.semester
		FROM teaching_courses tc JOIN academic_terms t ON t.id = tc.term_id
		WHERE tc.id = $1`, teachingCourseID).Scan(&termID, &academicYear, &semester)

	comp, err := s.buildExportRows(ctx, teachingCourseID, months)
	if err != nil {
		return nil, "", 0, err
	}
	courseCode := comp.courseCode
	records := comp.records

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	add := func(name string, body []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, bytes.NewReader(body))
		return err
	}

	// ONE combined workbook for the whole course, then one timetable per TA —
	// flat, no per-person folders.
	//
	// The old layout was a folder per TA holding a claim workbook per month plus
	// two PDFs. It matched how the data is shaped, not how the paperwork is
	// used: the office prints a course in one go, in a fixed order, and was
	// reassembling twenty files by hand every time. The combined book already
	// carries every person and every month in that order (see
	// export_combined_book.go), so the folders had nothing left to hold.
	//
	// The PDF coversheet and the PDF timetable are gone with them: both were
	// second renderings of what the workbooks already say, and a printed pack
	// containing two versions of one claim is a pack somebody has to reconcile.
	//
	// ป.ตรี and บัณฑิตศึกษา are claimed on SEPARATE paperwork — they are two
	// different forms, not one form with a level column, so each level's book
	// holds only its own people. A course with no undergrad TA gets no
	// undergrad book at all (nil, not an error).
	book, err := s.BuildCombinedClaimWorkbook(ctx, teachingCourseID, months)
	if err != nil {
		return nil, "", 0, err
	}
	if book != nil {
		if err := add(fmt.Sprintf("%s-เบิกจ่าย.xlsx", courseCode), book); err != nil {
			return nil, "", 0, err
		}
	}

	// บัณฑิตศึกษา files a DIFFERENT หลักฐานการจ่ายเงิน from ป.ตรี — one column
	// per month rather than a single total, and a เหมาจ่าย sheet with no hour or
	// rate columns at all (see export_grad_evidence.go). Its own workbook,
	// holding only graduate TAs, exactly as the college's own two example files
	// are filed.
	//
	// Nil, not an error, when the course has no graduate TAs: that is most
	// courses, and it must add nothing to their pack.
	gradEvidence, err := s.BuildGradEvidenceWorkbook(ctx, teachingCourseID, months)
	if err != nil {
		return nil, "", 0, err
	}
	if gradEvidence != nil {
		if err := add(fmt.Sprintf("%s-หลักฐาน-บัณฑิต.xlsx", courseCode), gradEvidence); err != nil {
			return nil, "", 0, err
		}
	}
	// Neither level produced a claim document: there is nothing to pay, and a
	// ZIP holding only timetables would look like a payout pack while billing
	// nobody. Same refusal the combined book used to raise on its own.
	if book == nil && gradEvidence == nil {
		return nil, "", 0, Invalid("ไม่มีบันทึกเวลาที่อนุมัติแล้วในวิชานี้ ยังสร้างเอกสารเบิกจ่ายไม่ได้")
	}

	// แบบแสดงรายละเอียดภาระงานของผู้ช่วยสอน — one .docx per grad-REGULAR TA,
	// the sheet the lecturer signs to say what those claimed hours were spent
	// on. Grad-special gets none: their pay is the flat term lump and they log
	// no hours to break down.
	forms, err := s.BuildGradWorkloadForms(ctx, teachingCourseID, months)
	if err != nil {
		return nil, "", 0, err
	}
	for _, form := range forms {
		if err := add(fmt.Sprintf("%s-ภาระงาน-%s.docx", courseCode, sanitize(form.FullName)), form.Doc); err != nil {
			return nil, "", 0, err
		}
	}

	// ตารางเรียนและตารางปฏิบัติงาน stays per person: it covers EVERY course the
	// TA assists, not just this one, which is the point of it — the signature
	// attests that the duties do not collide with the classes the TA has to
	// attend, and that cannot be judged one course at a time. Best-effort, so a
	// TA with no timetable does not sink the payout pack.
	if termID != uuid.Nil {
		for _, r := range records {
			tt, terr := s.BuildTimetableWorkbook(ctx, r.taID, termID)
			if terr != nil {
				continue
			}
			if err := add(fmt.Sprintf("%s-ตารางเรียน-%s.xlsx", courseCode, sanitize(r.fullName)), tt); err != nil {
				return nil, "", 0, err
			}
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", 0, err
	}
	// ปีการศึกษา_เทอม_รหัสวิชา — the way the finance office files these. It used
	// to carry a download timestamp, which made every re-download a differently
	// named copy of the same pack and left staff guessing which was current.
	// A term is exported once, so the term itself is the identity — EXCEPT once
	// the fiscal-year split (10/08/2026) means a term can be exported TWICE
	// (มิ.ย.–ก.ย. then ตุลาคม on a separate document): without the slice in the
	// name, both downloads land in a finance officer's folder under the exact
	// same filename, and the second silently overwrites the first.
	name := fmt.Sprintf("%d_%d_%s", academicYear, semester, courseCode)
	if academicYear == 0 {
		// No term on the course — fall back rather than ship "0_0_CODE.zip".
		name = courseCode
	}
	if len(months) > 0 {
		name += "_" + months[0]
		if len(months) > 1 {
			name += "_" + months[len(months)-1]
		}
	}
	name += ".zip"
	return buf.Bytes(), name, len(records), nil
}

// validatePayoutReadiness lists every TA in the course whose profile would
// produce a defective reimbursement document — missing profile row, profile
// not yet approved by staff, or blank national-ID/bank fields — and refuses
// the export with all offenders named in one message.
func (s *ExportService) validatePayoutReadiness(ctx context.Context, teachingCourseID uuid.UUID) error {
	// Readiness = an approved profile AND an approved creditor-form document.
	//
	// This used to test whether the bank columns were non-empty. Those columns
	// no longer exist (PDPA, migration 0047): the account number lives only in
	// the creditor-form PDF that ships with the finance package. So the honest
	// question is "does that PDF exist and has staff approved it?" — which is
	// also a stronger check, because a filled column never proved the document
	// had been produced.
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT u.first_name || ' ' || u.last_name,
		       (p.user_id IS NULL),
		       COALESCE(p.status::text, ''),
		       NOT EXISTS (
		           SELECT 1 FROM ta_documents d
		           WHERE d.user_id = a.ta_id AND d.kind = 'creditor_form'
		             AND d.superseded_at IS NULL AND d.status = 'approved')
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN ta_profiles p ON p.user_id = a.ta_id
		WHERE r.teaching_course_id = $1
		  AND (p.user_id IS NULL
		       OR p.status::text <> 'approved'
		       OR NOT EXISTS (
		           SELECT 1 FROM ta_documents d
		           WHERE d.user_id = a.ta_id AND d.kind = 'creditor_form'
		             AND d.superseded_at IS NULL AND d.status = 'approved'))
		ORDER BY 1`, teachingCourseID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var offenders []string
	for rows.Next() {
		var name, status string
		var noProfile, missingFields bool
		if err := rows.Scan(&name, &noProfile, &status, &missingFields); err != nil {
			return err
		}
		reason := ""
		switch {
		case noProfile:
			reason = "ยังไม่กรอกโปรไฟล์"
		case status != "approved":
			reason = "เอกสารยังไม่ผ่านการอนุมัติ"
		case missingFields:
			reason = "ยังไม่มีแบบฟอร์มเจ้าหนี้ที่อนุมัติแล้ว"
		}
		offenders = append(offenders, fmt.Sprintf("%s (%s)", name, reason))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(offenders) > 0 {
		return Invalid("ส่งออกไม่ได้ ข้อมูล TA ไม่ครบหรือยังไม่ผ่านการตรวจ: " + strings.Join(offenders, ", "))
	}
	return nil
}

// readinessByTA returns every TA's profile-readiness issue for the course,
// keyed by ta_id ("" = ready). Same per-row logic as validatePayoutReadiness
// but for ALL TAs (not just offenders) so the preview can flag individual rows
// without blocking the whole page.
func (s *ExportService) readinessByTA(ctx context.Context, teachingCourseID uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.ta_id,
		       (p.user_id IS NULL),
		       COALESCE(p.status::text, ''),
		       NOT EXISTS (
		           SELECT 1 FROM ta_documents d
		           WHERE d.user_id = a.ta_id AND d.kind = 'creditor_form'
		             AND d.superseded_at IS NULL AND d.status = 'approved')
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		LEFT JOIN ta_profiles p ON p.user_id = a.ta_id
		WHERE r.teaching_course_id = $1`, teachingCourseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var taID uuid.UUID
		var noProfile, missingFields bool
		var status string
		if err := rows.Scan(&taID, &noProfile, &status, &missingFields); err != nil {
			return nil, err
		}
		issue := ""
		switch {
		case noProfile:
			issue = "ยังไม่กรอกโปรไฟล์"
		case status != "approved":
			issue = "เอกสารยังไม่ผ่านการอนุมัติ"
		case missingFields:
			issue = "ยังไม่มีแบบฟอร์มเจ้าหนี้ที่อนุมัติแล้ว"
		}
		out[taID] = issue
	}
	return out, rows.Err()
}

// CoursePreview returns the read-only payout preview for a course: the exact
// per-TA numbers the ZIP export would contain, plus each TA's profile-readiness
// so staff can review (and fix) before the locking download. No side effects.
// months (Gregorian "YYYY-MM", empty = the whole term) previews one fiscal
// slice, so the figures on screen are the ones the download for those months
// would carry — the panel and the file must never disagree.
func (s *ExportService) CoursePreview(ctx context.Context, teachingCourseID uuid.UUID, months []string) (*ExportPreview, error) {
	comp, err := s.buildExportRows(ctx, teachingCourseID, months)
	if err != nil {
		return nil, err
	}
	ready, err := s.readinessByTA(ctx, teachingCourseID)
	if err != nil {
		return nil, err
	}
	blockers, err := s.CourseExportBlockers(ctx, teachingCourseID, months)
	if err != nil {
		return nil, err
	}
	out := &ExportPreview{
		TeachingCourseID: teachingCourseID.String(),
		CourseCode:       comp.courseCode,
		CourseNameTH:     comp.courseName,
		TermMonths:       comp.termMonths,
		BudgetMax:        comp.budgetMax,
		Prorated:         comp.prorated,
		Settlement:       comp.settlement,
		AllReady:         true,
		Blockers:         blockers,
		Rows:             []ExportPreviewRow{},
	}
	if out.Blockers == nil {
		out.Blockers = []ExportBlocker{}
	}
	for _, r := range comp.records {
		issue := ready[r.taID]
		if issue != "" {
			out.AllReady = false
		}
		out.TotalPay += r.payBaht
		out.TotalActual += r.actualPaid
		out.Rows = append(out.Rows, ExportPreviewRow{
			TAID:         r.taID.String(),
			FullName:     r.fullName,
			Email:        r.email,
			Level:        r.level,
			LevelTH:      levelLabelTH(r.level),
			Track:        r.track,
			TrackTH:      trackLabelTH(r.track),
			HoursTotal:   r.hoursTotal,
			PayBaht:      r.payBaht,
			ActualPaid:   r.actualPaid,
			IsReturning:  r.isReturning,
			ProfileReady: issue == "",
			ProfileIssue: issue,
		})
	}
	out.TotalPay = round2(out.TotalPay)
	out.TotalActual = round2(out.TotalActual)
	out.CanExport = out.AllReady && len(out.Blockers) == 0
	out.OverBudget = comp.budgetMax > 0 && out.TotalPay > comp.budgetMax+0.01
	return out, nil
}

func levelLabelTH(level string) string {
	switch level {
	case "undergrad":
		return "ปริญญาตรี"
	case "master":
		return "ปริญญาโท"
	case "phd":
		return "ปริญญาเอก"
	}
	return level
}

func trackLabelTH(track string) string {
	switch track {
	case "regular":
		return "ภาคปกติ"
	case "special":
		return "ภาคพิเศษ"
	}
	return track
}

func sanitize(s string) string {
	out := []rune{}
	for _, c := range s {
		switch c {
		case '/', '\\', '?', '*', ':', '"', '<', '>', '|':
			out = append(out, '_')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
