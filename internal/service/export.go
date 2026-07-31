package service

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/pdfgen"
	"ta-payment-back/internal/storage"
)

type ExportService struct {
	pool    *pgxpool.Pool
	store   storage.Store
	budget  *BudgetService
	fontDir string // path to Sarabun-Regular/Bold TTFs, used by the PDF renderer
	// teaching renders the weekly timetable form. Held as a dependency rather
	// than duplicated here so the zip ships the same document the TA prints.
	teaching *TeachingService
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
	TeachingCourseID string             `json:"teaching_course_id"`
	CourseCode       string             `json:"course_code"`
	CourseNameTH     string             `json:"course_name_th"`
	TermMonths       int                `json:"term_months"`
	BudgetMax        float64            `json:"budget_max"`
	TotalPay         float64            `json:"total_pay"`    // Σ pay_baht
	TotalActual      float64            `json:"total_actual"` // Σ actual_paid
	OverBudget       bool               `json:"over_budget"`
	Prorated         bool               `json:"prorated"`
	AllReady         bool               `json:"all_ready"` // false blocks the download
	Rows             []ExportPreviewRow `json:"rows"`
}

// round2 rounds a baht amount to 2 decimals — every figure that lands on a
// payroll document goes through this so float64 accumulation noise (e.g.
// 1234.5600000001) never reaches the spreadsheet.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// applyProrataCap is superseded by applyTrackProrataCap (two-pool, per-track
// budget). Retained for reference/tests; not used by buildExportRows anymore.
//
// applyProrataCap scales actualPaid on each row when Σ payBaht > budgetMax
// so that Σ actualPaid lands on budgetMax (rounding drift is settled on the
// trailing rows). Returns true if scaling was applied. Pure — no I/O, DB-free.
//
// Semantics:
//   - budgetMax ≤ 0 → no-op (unlimited).
//   - Σ payBaht ≤ budgetMax → no-op (each actualPaid stays at payBaht).
//   - Σ payBaht > budgetMax → each actualPaid = round2(payBaht × k) with
//     k = budgetMax / Σ payBaht; residual is folded into the trailing rows,
//     clamped to [0, payBaht] per row so no TA is paid a negative amount or
//     more than they earned. Σ actualPaid never exceeds budgetMax (it may
//     fall marginally short when clamping eats the residual).
func applyProrataCap(records []exportRow, budgetMax float64) bool {
	if budgetMax <= 0 || len(records) == 0 {
		return false
	}
	var totalRaw float64
	for _, r := range records {
		totalRaw += r.payBaht
	}
	if totalRaw <= budgetMax+0.01 {
		return false
	}
	k := budgetMax / totalRaw
	var scaledSum float64
	for i := range records {
		records[i].actualPaid = round2(records[i].payBaht * k)
		scaledSum += records[i].actualPaid
	}
	// Fold the residual (positive or negative) into the trailing rows, clamping
	// each row to [0, payBaht]. Walk backwards so the adjustment stays
	// deterministic; k < 1 keeps every row strictly below payBaht, so a small
	// positive residual almost always fits on the last row alone.
	residual := round2(budgetMax - scaledSum)
	for i := len(records) - 1; i >= 0 && residual != 0; i-- {
		adjusted := round2(records[i].actualPaid + residual)
		if adjusted < 0 {
			adjusted = 0
		}
		if adjusted > records[i].payBaht {
			adjusted = records[i].payBaht
		}
		residual = round2(residual - (adjusted - records[i].actualPaid))
		records[i].actualPaid = adjusted
	}
	return true
}

// applyTrackProrataCap caps each track's pay against its own budget pool:
// regular-track pay at regularPool (งบภาคปกติ) and special-track pay at
// specialPool (งบภาคพิเศษ), independently. Within a track, if that track's
// total earned exceeds its pool, every row's share of that track is scaled by
// k_track = pool / Σ(track pay). Each row's actualPaid = scaled regular +
// scaled special. Returns true if either track was scaled. A pool ≤ 0 means
// "unlimited" for that track (no data → don't zero people out). Pure, DB-free.
func applyTrackProrataCap(records []exportRow, regularPool, specialPool, spillableReg float64) bool {
	var sumReg, sumSpec float64
	for _, r := range records {
		sumReg += r.payRegular
		sumSpec += r.paySpecial
	}
	// Concurrent-section spill (B2): when the regular pool is exhausted, the
	// regular pay attributable to overlap TAs (spillableReg) may overflow into
	// the special pool's UNUSED capacity — "เบิกภาคปกติก่อน ถ้างบปกติหมดค่อยไหล
	// ไปพิเศษ". Non-overlap regular pay never borrows from special.
	effReg, effSpec := regularPool, specialPool
	if regularPool > 0 && specialPool > 0 && spillableReg > 0 && sumReg > regularPool+0.01 {
		spill := sumReg - regularPool // regular shortfall
		if unused := specialPool - sumSpec; unused < spill {
			spill = unused
		}
		if spillableReg < spill {
			spill = spillableReg
		}
		if spill > 0 {
			effReg = regularPool + spill
			effSpec = specialPool - spill
		}
	}
	regScale, specScale := 1.0, 1.0
	if effReg > 0 && sumReg > effReg+0.01 {
		regScale = effReg / sumReg
	}
	if effSpec > 0 && sumSpec > effSpec+0.01 {
		specScale = effSpec / sumSpec
	}
	scaled := regScale < 1 || specScale < 1
	for i := range records {
		records[i].actualPaid = round2(records[i].payRegular*regScale + records[i].paySpecial*specScale)
	}
	return scaled
}

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
// Billing uses two independent budget pools (ภาคปกติ / ภาคพิเศษ), each capped
// separately (applyTrackProrataCap). Concurrent-section rule (B2): when an
// undergrad TA logs regular AND special work at the SAME clock time, the
// overlapping hours are counted ONCE at the regular rate ("เบิกภาคปกติก่อน") —
// the duplicated hours are removed from the special side after aggregation.
// Grad tiers are unaffected (grad-special is a flat lump).
// buildExportRows performs the full payout computation for a course (pay rates,
// overlap dedup, grad tiers, two-pool pro-rata budget cap) and returns the
// priced rows without any side effects or readiness gate. Both the ZIP builder
// and the read-only preview call this so their numbers can never drift.
func (s *ExportService) buildExportRows(ctx context.Context, teachingCourseID uuid.UUID) (*exportComputation, error) {
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

	// Pull per-assignment rows; each is billed at its own track's rate into the
	// regular or special pool (two-pool cap applied after aggregation).
	assignRows, err := s.pool.Query(ctx, `
		SELECT a.id, a.ta_id, u.first_name||' '||u.last_name, u.email,
		       sec.track::text, a.level::text,
		       COALESCE(SUM(wl.hours) FILTER (WHERE wl.status='approved'), 0) AS approved_hrs,
		       (SELECT COALESCE(SUM(EXTRACT(EPOCH FROM (end_time-start_time))/3600),0)
		        FROM section_schedules WHERE section_id=sec.id) AS sched_hrs_per_week
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status='approved'
		JOIN users u ON u.id = a.ta_id
		JOIN sections sec ON sec.id = a.section_id
		LEFT JOIN work_logs wl ON wl.assignment_id = a.id
		WHERE r.teaching_course_id = $1
		GROUP BY a.id, a.ta_id, u.first_name, u.last_name, u.email,
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

		// Bill each assignment at its own track's rate and accumulate into the
		// matching pool. Regular-track work → payRegular; special-track work →
		// paySpecial. The two pools are capped independently downstream.
		switch {
		case level == "undergrad" && track == "regular":
			agg.payRegular += approvedHrs * pr.UndergradRegular

		case level == "undergrad" && track == "special":
			agg.paySpecial += approvedHrs * pr.UndergradSpecial

		case (level == "master" || level == "phd") && track == "regular":
			// Q&A rule 6c: บัณฑิต regular is HOURLY (50฿/hr per ประกาศ).
			agg.payRegular += approvedHrs * pr.GraduateRegularHourly

		case (level == "master" || level == "phd") && track == "special":
			// Q&A rule 6b: บัณฑิต special is a flat lump (special-track pool),
			// added once per TA after the loop (see below).
			agg.hasGradSpecial = true
		}
	}

	// Grad-special lump sum: add once per TA (not per section), and only when the
	// TA actually has approved work in the course. See the switch above.
	gradLump := pr.GraduateSpecialLumpsum * float64(termMonths)
	if pr.GradSpecialTermCap > 0 && gradLump > pr.GradSpecialTermCap {
		gradLump = pr.GradSpecialTermCap
	}
	for _, taID := range order {
		agg := byTA[taID]
		if agg.hasGradSpecial && agg.hoursTotal > 0 {
			agg.paySpecial += gradLump
		}
	}

	// Concurrent-section rule (B2): when an undergrad TA logs regular AND special
	// work at the SAME clock time (time-overlapping sections), those overlapping
	// hours are counted ONCE — the time is already paid on the regular side (at
	// the regular rate = "เบิกภาคปกติก่อน"), so remove the duplicated hours from
	// the special side. Only undergrad (both tracks hourly); grad-special is a
	// flat lump so it is unaffected.
	overlapRows, oerr := s.pool.Query(ctx, `
		SELECT a1.ta_id,
		       SUM(GREATEST(0, EXTRACT(EPOCH FROM (
		           LEAST(w1.end_time, w2.end_time) - GREATEST(w1.start_time, w2.start_time)
		       )) / 3600.0)) AS overlap_hrs
		FROM work_logs w1
		JOIN ta_request_assignments a1 ON a1.id = w1.assignment_id
		JOIN sections s1 ON s1.id = a1.section_id AND s1.track = 'regular'
		JOIN ta_request_assignments a2 ON a2.ta_id = a1.ta_id
		JOIN sections s2 ON s2.id = a2.section_id AND s2.track = 'special'
		JOIN work_logs w2 ON w2.assignment_id = a2.id AND w2.work_date = w1.work_date
		WHERE s1.teaching_course_id = $1 AND s2.teaching_course_id = $1
		  AND w1.status = 'approved' AND w2.status = 'approved'
		  AND w1.start_time < w2.end_time AND w2.start_time < w1.end_time
		GROUP BY a1.ta_id`, teachingCourseID)
	if oerr != nil {
		return nil, oerr
	}
	overlapByTA := map[uuid.UUID]float64{}
	for overlapRows.Next() {
		var taID uuid.UUID
		var hrs float64
		if err := overlapRows.Scan(&taID, &hrs); err != nil {
			overlapRows.Close()
			return nil, err
		}
		overlapByTA[taID] = hrs
	}
	overlapRows.Close()
	if err := overlapRows.Err(); err != nil {
		return nil, err
	}
	var spillableReg float64
	for _, taID := range order {
		agg := byTA[taID]
		if agg.level != "undergrad" {
			continue
		}
		if oh := overlapByTA[taID]; oh > 0 {
			deduct := oh * pr.UndergradSpecial
			if deduct > agg.paySpecial {
				deduct = agg.paySpecial
			}
			agg.paySpecial -= deduct
			// Regular pay for the overlap hours may spill into the special pool
			// if the regular budget runs out (see applyTrackProrataCap).
			spillableReg += oh * pr.UndergradRegular
		}
	}

	// ป.ตรี ภาคพิเศษ: ประกาศกำหนด "50 ฿/ชม. หรือ 2,000 ฿/เดือน" — จ่ายรายชั่วโมง
	// ตามจริงแต่ไม่เกินเพดานรายเดือน คิดแยกทีละเดือน (เดือนที่ทำเกินไม่ไปหักลบ
	// กับเดือนที่ทำน้อย) แล้วรวมเป็นยอดของเทอม. ทำหลังหักชั่วโมงซ้อน (B2) เพื่อ
	// ไม่ให้ชั่วโมงที่ถูกตัดออกไปแล้วมากินเพดาน.
	if pr.UGSpecialMonthlyCap > 0 {
		capRows, cerr := s.pool.Query(ctx, `
			SELECT monthly.ta_id, SUM(LEAST(monthly.hrs * $2, $3)) AS capped_pay
			FROM (
			    SELECT a.ta_id AS ta_id, to_char(wl.work_date,'YYYY-MM') AS ym,
			           SUM(wl.hours) AS hrs
			    FROM work_logs wl
			    JOIN ta_request_assignments a ON a.id = wl.assignment_id
			    JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
			    JOIN sections sec ON sec.id = a.section_id AND sec.track = 'special'
			    WHERE r.teaching_course_id = $1 AND wl.status = 'approved'
			      AND a.level = 'undergrad'
			    GROUP BY a.ta_id, to_char(wl.work_date,'YYYY-MM')
			) monthly
			GROUP BY monthly.ta_id`, teachingCourseID, pr.UndergradSpecial, pr.UGSpecialMonthlyCap)
		if cerr != nil {
			return nil, cerr
		}
		cappedByTA := map[uuid.UUID]float64{}
		for capRows.Next() {
			var taID uuid.UUID
			var capped float64
			if err := capRows.Scan(&taID, &capped); err != nil {
				capRows.Close()
				return nil, err
			}
			cappedByTA[taID] = capped
		}
		capRows.Close()
		if err := capRows.Err(); err != nil {
			return nil, err
		}
		for _, taID := range order {
			agg := byTA[taID]
			if agg.level != "undergrad" {
				continue
			}
			if capped, ok := cappedByTA[taID]; ok && agg.paySpecial > capped {
				agg.paySpecial = capped
			}
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
	var budgetMax, regularPool, specialPool float64
	if s.budget != nil {
		if snap, err := s.budget.Compute(ctx, teachingCourseID); err == nil {
			budgetMax = snap.PerCourseMaxBaht
			regularPool = snap.TermPayRegular
			specialPool = snap.TermPaySpecial
		}
	}
	prorata := applyTrackProrataCap(records, regularPool, specialPool, spillableReg)

	return &exportComputation{
		courseCode: courseCode, courseName: courseName,
		termMonths: termMonths, records: records,
		budgetMax: budgetMax, prorated: prorata,
	}, nil
}

// BuildCourseZip builds the per-TA .xlsx (+ best-effort .pdf) ZIP for a course.
// It gates on payout readiness, then reuses buildExportRows so the file numbers
// match the preview exactly. Returns (zip bytes, filename, TA count, error).
func (s *ExportService) BuildCourseZip(ctx context.Context, teachingCourseID uuid.UUID) ([]byte, string, int, error) {
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
		return nil, "", 0, Invalid("ยังไม่ได้กรอกจำนวนนักศึกษาของวิชานี้ — กรุณากรอกที่หน้า “วิชาที่เปิดสอน” ก่อนส่งออก (งบเบิกจ่ายคำนวณจากจำนวนนักศึกษา)")
	}
	// Payout readiness gate: refuse to build reimbursement documents while any
	// TA in the course has an unapproved or incomplete profile — otherwise the
	// coversheets ship with blank national-ID/bank fields and nobody notices
	// until the finance office bounces the batch.
	if err := s.validatePayoutReadiness(ctx, teachingCourseID); err != nil {
		return nil, "", 0, err
	}
	// Needed by the timetable form, which is scoped to a TERM rather than a
	// course. A missing term is not fatal — the form is simply skipped.
	var termID uuid.UUID
	_ = s.pool.QueryRow(ctx,
		`SELECT term_id FROM teaching_courses WHERE id=$1`, teachingCourseID).Scan(&termID)

	comp, err := s.buildExportRows(ctx, teachingCourseID)
	if err != nil {
		return nil, "", 0, err
	}
	courseCode, courseName := comp.courseCode, comp.courseName
	records := comp.records

	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	// Folder convention per ประกาศ 2569:
	//   {course_code}/{TA name}/{course_code} - {TA name}.xlsx
	//   {course_code}/{TA name}/{course_code} - {TA name}.pdf
	// (Phase 3 will add /{month} between name and file when the export scopes
	// to a specific submission period.)
	for _, r := range records {
		safeName := sanitize(r.fullName)
		folder := fmt.Sprintf("%s/%s", courseCode, safeName)
		fileBase := fmt.Sprintf("%s - %s", courseCode, safeName)

		// The official claim workbook, one per term month with activity —
		// the exact document the college signs. This replaced the old
		// system-invented summary .xlsx outright (31/07/2026): shipping both
		// meant two documents claiming to be "the" reimbursement workbook,
		// and only the monthly one matches what the faculty actually files.
		// Months with no logged work produce no file.
		if termID != uuid.Nil {
			tstart, tend := time.Time{}, time.Time{}
			_ = s.pool.QueryRow(ctx, `SELECT starts_on, ends_on FROM academic_terms WHERE id=$1`, termID).Scan(&tstart, &tend)
			idx := 0
			for ym := time.Date(tstart.Year(), tstart.Month(), 1, 0, 0, 0, 0, time.UTC); !ym.After(tend); ym = ym.AddDate(0, 1, 0) {
				idx++
				logs, lerr := s.claimLogs(ctx, r.taID, teachingCourseID, ym.Year(), int(ym.Month()))
				if lerr != nil || len(logs) == 0 {
					continue
				}
				book, berr := s.BuildClaimWorkbook(ctx, r.taID, teachingCourseID, ym.Year(), int(ym.Month()))
				if berr != nil {
					continue // best-effort, like the PDF below
				}
				bn := fmt.Sprintf("%s/%d_%s-%s-%s.xlsx", folder, idx, courseCode, safeName, thaiMonthNames[int(ym.Month())])
				bw, werr := zw.Create(bn)
				if werr != nil {
					return nil, "", 0, werr
				}
				if _, werr := io.Copy(bw, bytes.NewReader(book)); werr != nil {
					return nil, "", 0, werr
				}
			}
		}

		// PDF is best-effort: without a fontDir configured we skip it rather
		// than fail the whole export. The zip still ships the .xlsx.
		if s.fontDir != "" {
			pdfBytes, perr := s.buildPerTAPDF(ctx, teachingCourseID, courseCode, courseName, "", r)
			if perr == nil {
				pw, err := zw.Create(folder + "/" + fileBase + ".pdf")
				if err != nil {
					return nil, "", 0, err
				}
				if _, err := io.Copy(pw, bytes.NewReader(pdfBytes)); err != nil {
					return nil, "", 0, err
				}
			}
		}

		// ตารางเรียนและตารางปฏิบัติงาน — the weekly form the lecturer signs.
		// It covers EVERY course the TA assists, not just this one, which is the
		// point of it: the signature attests that the duties do not collide with
		// the classes the TA has to attend, and that cannot be judged one course
		// at a time. Best-effort for the same reason as above.
		if s.teaching != nil && termID != uuid.Nil {
			if tf, terr := s.teaching.BuildTimetableFormPDF(ctx, r.taID, termID, ""); terr == nil {
				tw, err := zw.Create(folder + "/ตารางปฏิบัติงาน - " + safeName + ".pdf")
				if err != nil {
					return nil, "", 0, err
				}
				if _, err := io.Copy(tw, bytes.NewReader(tf)); err != nil {
					return nil, "", 0, err
				}
			}
		}
	}
	if err := zw.Close(); err != nil {
		return nil, "", 0, err
	}
	name := fmt.Sprintf("%s_%s.zip", courseCode, time.Now().Format("20060102_150405"))
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
		return Invalid("ส่งออกไม่ได้ — ข้อมูล TA ไม่ครบหรือยังไม่ผ่านการตรวจ: " + strings.Join(offenders, ", "))
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
func (s *ExportService) CoursePreview(ctx context.Context, teachingCourseID uuid.UUID) (*ExportPreview, error) {
	comp, err := s.buildExportRows(ctx, teachingCourseID)
	if err != nil {
		return nil, err
	}
	ready, err := s.readinessByTA(ctx, teachingCourseID)
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
		AllReady:         true,
		Rows:             []ExportPreviewRow{},
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

// buildPerTAPDF assembles the monthly worklog PDF for one TA. The
// yearMonthLabel is optional (empty = "รวมทุกเดือน") and is used to filter
// the approved worklog entries embedded on the sheet.
func (s *ExportService) buildPerTAPDF(ctx context.Context, tcID uuid.UUID,
	code, name, yearMonthLabel string,
	r exportRow) ([]byte, error) {

	// Human-friendly level/track labels for the coversheet.
	levelTH := r.level
	switch r.level {
	case "undergrad":
		levelTH = "ปริญญาตรี"
	case "master":
		levelTH = "ปริญญาโท"
	case "phd":
		levelTH = "ปริญญาเอก"
	}
	trackTH := r.track
	switch r.track {
	case "regular":
		trackTH = "ภาคปกติ"
	case "special":
		trackTH = "ภาคพิเศษ"
	}

	// Schedule (Block B) — combine all sections this TA holds for the course.
	sched := []pdfgen.ScheduleRow{}
	dayName := []string{"อาทิตย์", "จันทร์", "อังคาร", "พุธ", "พฤหัส", "ศุกร์", "เสาร์"}
	rows, _ := s.pool.Query(ctx, `
		SELECT ss.day_of_week, ss.start_time::text, ss.end_time::text, ss.kind, COALESCE(ss.room,'')
		FROM section_schedules ss
		JOIN sections sec ON sec.id = ss.section_id
		JOIN ta_request_assignments a ON a.section_id = sec.id
		JOIN ta_requests req ON req.id = a.request_id AND req.status='approved'
		WHERE req.teaching_course_id = $1 AND a.ta_id = $2
		ORDER BY ss.day_of_week, ss.start_time`, tcID, r.taID)
	for rows.Next() {
		var day int
		var start, end, kind, room string
		if err := rows.Scan(&day, &start, &end, &kind, &room); err == nil {
			kindTH := kind
			if kind == "lecture" {
				kindTH = "บรรยาย"
			} else if kind == "lab" {
				kindTH = "ปฏิบัติการ"
			}
			d := ""
			if day >= 0 && day < len(dayName) {
				d = dayName[day]
			}
			sched = append(sched, pdfgen.ScheduleRow{
				DayTH: d, StartTime: start, EndTime: end, Kind: kindTH, Room: room,
			})
		}
	}
	rows.Close()

	// Approved worklog entries (Block C).
	entries := []pdfgen.WorklogRow{}
	entryRows, _ := s.pool.Query(ctx, `
		SELECT TO_CHAR(wl.work_date,'YYYY-MM-DD'), wl.start_time::text, wl.end_time::text,
		       wl.hours, wl.activity, COALESCE(wl.room,''), COALESCE(wl.note,'')
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN ta_requests req ON req.id = a.request_id
		WHERE req.teaching_course_id = $1 AND a.ta_id = $2 AND wl.status = 'approved'
		ORDER BY wl.work_date, wl.start_time`, tcID, r.taID)
	for entryRows.Next() {
		var date, start, end, activity, room, note string
		var hours float64
		if err := entryRows.Scan(&date, &start, &end, &hours, &activity, &room, &note); err == nil {
			label := activity
			switch activity {
			case "lecture":
				label = "บรรยาย"
			case "lab":
				label = "ปฏิบัติการ"
			case "review":
				label = "ตรวจงาน"
			case "makeup":
				label = "ชดเชย"
			case "other":
				label = "อื่นๆ"
			}
			entries = append(entries, pdfgen.WorklogRow{
				Date: date, Start: start, End: end, Hours: hours,
				Activity: label, Room: room, Note: note,
			})
		}
	}
	entryRows.Close()

	if yearMonthLabel == "" {
		yearMonthLabel = "รวมทุกเดือน"
	}

	return pdfgen.BuildWorklogPDF(pdfgen.WorklogPDFInput{
		FontDir: s.fontDir,
		Data: pdfgen.WorklogPDFData{
			CourseCode: code, CourseName: name,
			YearMonthLabel: yearMonthLabel,
			FullName:       r.fullName,
			Email:          r.email,
			Level:          levelTH,
			Track:          trackTH,
			HoursTotal:     r.hoursTotal,
			PayBaht:        r.payBaht,
			Schedule:       sched,
			Entries:        entries,
		},
	})
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
