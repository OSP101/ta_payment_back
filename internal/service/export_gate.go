package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"ta-payment-back/internal/timeutil"
)

// ExportBlocker is one reason a course may not be exported yet, phrased for the
// staff screen rather than for a log.
type ExportBlocker struct {
	// Kind is "waiting_ta" | "waiting_lecturer" | "class_clash" | "not_appointed" | "unreviewed" | "not_exported".
	Kind   string `json:"kind"`
	TAName string `json:"ta_name"`
	// Months affected, as Thai labels ("สิงหาคม 2569").
	Months []string `json:"months"`
	Rows   int      `json:"rows,omitempty"`
	// CourseCode is set only by a term-wide gate (TermExportBlockers) — a
	// single-course gate leaves it empty since the caller already knows which
	// course they asked about.
	CourseCode string `json:"course_code,omitempty"`
}

// CourseExportBlockers reports every stage of the pipeline this course has not
// finished yet. Empty means the ZIP may be built.
//
// The three stages that must ALL be done, in order:
//
//  1. the TA sends the month          → waiting_ta
//  2. the lecturer approves it        → waiting_lecturer
//  3. staff sign the month off (ตรวจสอบเบิกจ่าย) → unreviewed
//
// Until 04/08/2026 the export checked none of them — only that student counts
// and TA profiles existed. That let staff download a payout document while work
// was still moving, and the damage was silent both ways: months staff had never
// signed off were BILLED in the file, and MarkCourseExported then refused to
// lock them (it requires staff_reviewed), so finance held a claim document whose
// worklogs stayed editable — the exact failure the locking step exists to
// prevent. The button offered what the rest of the system refused.
//
// Forfeited rows — unsent when their period closed — are not blockers. Nobody
// can move them, so treating them as outstanding would wedge the course shut
// forever (see outstandingRowSQL).
//
// months (Gregorian "YYYY-MM", empty = every month) narrows the gate to the
// fiscal slice being claimed. งบแผ่นดิน closes 30 กันยายน while ภาคต้น teaches
// มิ.ย.–ต.ค., so the มิ.ย.–ก.ย. claim has to be issuable in September, with
// October still being taught and necessarily unfinished.
func (s *ExportService) CourseExportBlockers(ctx context.Context, courseID uuid.UUID, months []string) ([]ExportBlocker, error) {
	rows, err := s.pool.Query(ctx, `
		WITH months AS (
		    SELECT a.ta_id,
		           COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		           COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'') AS ta_name,
		           -- Sorted on separately: gluing the คำนำหน้า onto the display
		           -- name would sort every นางสาว above every นาย, which is not
		           -- an order anyone reading this list is looking for.
		           COALESCE(u.first_name,'') AS sort_name,
		           sp.year_month,
		           COALESCE(st.status,'pending') AS staff_status,
		           COUNT(*) FILTER (WHERE `+waitingTASQL("wl")+`)       AS waiting_ta,
		           COUNT(*) FILTER (WHERE `+waitingLecturerSQL("wl")+`) AS waiting_lecturer,
		           COUNT(*) FILTER (WHERE wl.status = 'approved')       AS approved,
		           -- Only used to NAME the reason. The row blocks exactly as it
		           -- always did; an un-appointed pair's month simply cannot be
		           -- signed off, so "ยังไม่ได้ตรวจสอบเบิกจ่าย" sent staff to a
		           -- review queue that does not list them.
		           `+AppointedSQL("tc.id", "a.ta_id")+`                AS appointed
		    FROM teaching_courses tc
		    JOIN academic_terms trm ON trm.id = tc.term_id
		    JOIN submission_periods sp ON sp.term_id = tc.term_id
		    JOIN sections sec          ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		    JOIN users u ON u.id = a.ta_id
		    LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		    JOIN work_logs wl ON wl.assignment_id = a.id
		     AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		    LEFT JOIN submission_period_status st
		      ON st.submission_period_id = sp.id
		     AND st.ta_id = a.ta_id
		     AND st.teaching_course_id = tc.id
		    WHERE tc.id = $1
		      AND `+monthFilterSQL("wl.work_date", "$2")+`
		      -- Grad-special (master/phd on a special-track section) no longer
		      -- logs work_logs at all — pay is computed automatically from the
		      -- regular track's class schedule. Any work_logs rows left over from
		      -- before that change can never move again (nobody submits or
		      -- approves them), so counting them here would block this course's
		      -- export forever.
		      AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		    GROUP BY 1, 2, 3, 4, 5, a.ta_id, tc.id
		)
		SELECT ta_name, year_month, staff_status, waiting_ta, waiting_lecturer, approved, appointed
		FROM months
		WHERE waiting_ta > 0 OR waiting_lecturer > 0
		   OR (approved > 0 AND staff_status NOT IN ('staff_reviewed','exported','finance_sent'))
		ORDER BY sort_name, ta_name, year_month`, courseID, months)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// One blocker per (person, stage) — a TA with three unsent months is one
	// line naming three months, not three lines.
	type key struct{ kind, name string }
	agg := map[key]*ExportBlocker{}
	var order []key
	add := func(kind, name, ym string, n int) {
		k := key{kind, name}
		b, ok := agg[k]
		if !ok {
			b = &ExportBlocker{Kind: kind, TAName: name}
			agg[k] = b
			order = append(order, k)
		}
		b.Months = append(b.Months, ym)
		b.Rows += n
	}
	for rows.Next() {
		var name, ym, staffStatus string
		var waitingTA, waitingLecturer, approved int
		var appointed bool
		if err := rows.Scan(&name, &ym, &staffStatus, &waitingTA, &waitingLecturer, &approved, &appointed); err != nil {
			return nil, err
		}
		if waitingTA > 0 {
			add("waiting_ta", name, ym, waitingTA)
		}
		if waitingLecturer > 0 {
			add("waiting_lecturer", name, ym, waitingLecturer)
		}
		// Only flag an unreviewed month once nothing is still moving inside it;
		// otherwise the same month reads as two problems when it is one.
		if waitingTA == 0 && waitingLecturer == 0 && approved > 0 &&
			staffStatus != StatusStaffReviewed && staffStatus != "exported" && staffStatus != "finance_sent" {
			if appointed {
				add("unreviewed", name, ym, 0)
			} else {
				add("not_appointed", name, ym, 0)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	clashes, err := s.approvedClassClashes(ctx, courseID, months)
	if err != nil {
		return nil, err
	}
	for _, c := range clashes {
		add("class_clash", c.name, c.yearMonth, c.rows)
	}

	// Stage order, then name — staff read this list top-down as a work queue.
	rank := map[string]int{"waiting_ta": 0, "waiting_lecturer": 1, "class_clash": 2, "not_appointed": 3, "unreviewed": 4}
	sort.SliceStable(order, func(i, j int) bool {
		if rank[order[i].kind] != rank[order[j].kind] {
			return rank[order[i].kind] < rank[order[j].kind]
		}
		return order[i].name < order[j].name
	})
	out := make([]ExportBlocker, 0, len(order))
	for _, k := range order {
		b := agg[k]
		b.Months = thaiMonthLabelsBE(b.Months)
		out = append(out, *b)
	}
	return out, nil
}

// exportBlockedError turns the blocker list into the sentence staff sees when
// they press download anyway (a stale tab, or a direct call to the endpoint).
func exportBlockedError(blockers []ExportBlocker) error {
	lines := make([]string, 0, len(blockers))
	for _, b := range blockers {
		months := strings.Join(b.Months, ", ")
		name := b.TAName
		if b.CourseCode != "" {
			name = fmt.Sprintf("[%s] %s", b.CourseCode, b.TAName)
		}
		switch b.Kind {
		case "waiting_ta":
			lines = append(lines, fmt.Sprintf("%s ยังไม่ส่งบันทึกเวลา %d รายการ (%s)", name, b.Rows, months))
		case "waiting_lecturer":
			lines = append(lines, fmt.Sprintf("%s รออาจารย์อนุมัติ %d รายการ (%s)", name, b.Rows, months))
		case "not_exported":
			lines = append(lines, fmt.Sprintf("%s ตรวจสอบแล้วแต่ยังไม่ได้ส่งออกใบเบิกจ่าย (%s)", name, months))
		case "class_clash":
			lines = append(lines, fmt.Sprintf("%s มีรายการที่อนุมัติแล้วตรงกับตารางเรียนปัจจุบันของ TA %d รายการ — ตีกลับหรือแก้ไขก่อน (%s)", name, b.Rows, months))
		case "not_appointed":
			lines = append(lines, fmt.Sprintf("%s ยังไม่อยู่ในคำสั่งแต่งตั้ง — ออกคำสั่งรอบถัดไปก่อน (%s)", name, months))
		default:
			lines = append(lines, fmt.Sprintf("%s ยังไม่ได้ตรวจสอบเบิกจ่าย (%s)", name, months))
		}
	}
	return Invalid("ยังส่งออกไม่ได้ ต้องผ่านครบทุกขั้นก่อน:\n• " + strings.Join(lines, "\n• "))
}

// TermExportBlockers is the ประตู before generating ปะหน้าจ่ายตรง
// (transfer-cover): every course in the term must have reached 'exported' —
// the claim documents issued and the month locked, so the figures on this
// sheet can no longer move.
//
// It used to require finance_sent, one stage further on, and that was a dead
// end in two ways (08/09/2026). Ordering: this document IS what the finance
// office keys into ERP, so it has to be producible BEFORE the handoff, not
// after somebody has recorded that the handoff already happened. And in
// practice: 'ส่งการเงิน' has no button anywhere in the staff UI, so no month
// could ever reach finance_sent and the transfer cover was unobtainable for
// every term. Waiting on a stage the product cannot perform is not a gate,
// it is a wall.
//
// 'exported' is the right line to draw because it is the one that FREEZES the
// figures: MarkCourseExported locks the month, and the worklog editor refuses
// an exported month. Everything the sheet reports is final at that point.
//
// Scoped to the WHOLE TERM rather than one curriculum: the file bundles every
// curriculum that has data into one workbook, and letting some curricula's
// sheets print while others are still mid-review would produce a "complete"
// looking file that silently omits a curriculum — the exact half-finished
// output the plan explicitly rules out.
//
// months (Gregorian "YYYY-MM", empty = the whole term) narrows the gate to the
// slice being issued. Required by the fiscal-year split (10/08/2026): งบ closes
// 30 กันยายน, so the มิ.ย.–ก.ย. document has to be issuable IN September, while
// October is still being taught and could not possibly have been exported. A
// term-wide gate makes that document unobtainable until after the budget year
// it belongs to has already closed.
//
// level ("undergrad" | "graduate", 12/08/2026) narrows the gate to the FILE
// being issued: ปะหน้าจ่ายตรง is now two separate documents, and a graduate
// course still mid-review must not hold the undergrad file's export shut, nor
// the reverse. Note this means a term whose graduate TAs are ALL grad-special
// (no work_logs at all — see gradSpecialTAIDs) reports zero blockers for
// level="graduate" from the very first day of the term: there is nothing
// left to review before the lump is transferable. That is intentional, not a
// gap — see the warning BuildTransferCoverWorkbook attaches in that case.
//
// Filtered on work_date's own Gregorian month, not on sp.year_month, which
// carries a BUDDHIST academic year ("2569-06") and cannot be compared with a
// caller's Gregorian selection — see term_months.go.
// TermMonthsNotReady names the months of a term that still have something
// outstanding at this level — Gregorian keys, so the month picker can key off
// them directly.
//
// Same predicate as TermExportBlockers, asked per month instead of per person.
// The gate on the download has always been able to say WHY a term is not ready;
// what it could not do is say WHICH MONTHS, early enough for the picker to stop
// the officer choosing them. Pressing a button and being handed a wall of names
// is a worse way to learn that ตุลาคม is not signed off than seeing ตุลาคม
// greyed out before pressing anything.
func (s *ExportService) TermMonthsNotReady(ctx context.Context, termID uuid.UUID, level string) (map[string]bool, error) {
	if level != "undergrad" && level != "graduate" {
		return nil, fmt.Errorf("TermMonthsNotReady: invalid level %q", level)
	}
	rows, err := s.pool.Query(ctx, `
		WITH months AS (
		    SELECT sp.year_month,
		           COALESCE(st.status,'pending') AS staff_status,
		           COUNT(*) FILTER (WHERE `+waitingTASQL("wl")+`)       AS waiting_ta,
		           COUNT(*) FILTER (WHERE `+waitingLecturerSQL("wl")+`) AS waiting_lecturer,
		           COUNT(*) FILTER (WHERE wl.status = 'approved')       AS approved
		    FROM teaching_courses tc
		    JOIN academic_terms trm ON trm.id = tc.term_id
		    JOIN submission_periods sp ON sp.term_id = tc.term_id
		    JOIN sections sec          ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		    JOIN work_logs wl ON wl.assignment_id = a.id
		     AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		    LEFT JOIN submission_period_status st
		      ON st.submission_period_id = sp.id
		     AND st.ta_id = a.ta_id
		     AND st.teaching_course_id = tc.id
		    WHERE tc.term_id = $1
		      AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		      AND CASE WHEN $2 = 'graduate' THEN a.level::text IN ('master','phd')
		               ELSE a.level::text = 'undergrad' END
		    GROUP BY sp.year_month, st.ta_id, tc.id, st.status
		)
		SELECT DISTINCT year_month
		FROM months
		WHERE waiting_ta > 0 OR waiting_lecturer > 0
		   OR (approved > 0 AND staff_status NOT IN ('exported','finance_sent'))`, termID, level)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var buddhist string
		if err := rows.Scan(&buddhist); err != nil {
			return nil, err
		}
		// submission_periods.year_month is the BUDDHIST academic key ("2569-06");
		// everything the picker speaks in is Gregorian. See term_months.go.
		greg, err := gregorianYearMonth(buddhist)
		if err != nil {
			return nil, err
		}
		out[greg] = true
	}
	return out, rows.Err()
}

func (s *ExportService) TermExportBlockers(ctx context.Context, termID uuid.UUID, months []string, level string) ([]ExportBlocker, error) {
	if level != "undergrad" && level != "graduate" {
		return nil, fmt.Errorf("TermExportBlockers: invalid level %q", level)
	}
	rows, err := s.pool.Query(ctx, `
		WITH months AS (
		    SELECT tc.code AS course_code,
		           a.ta_id,
		           COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		           COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'') AS ta_name,
		           -- Sorted on separately: gluing the คำนำหน้า onto the display
		           -- name would sort every นางสาว above every นาย, which is not
		           -- an order anyone reading this list is looking for.
		           COALESCE(u.first_name,'') AS sort_name,
		           sp.year_month,
		           COALESCE(st.status,'pending') AS staff_status,
		           COUNT(*) FILTER (WHERE `+waitingTASQL("wl")+`)       AS waiting_ta,
		           COUNT(*) FILTER (WHERE `+waitingLecturerSQL("wl")+`) AS waiting_lecturer,
		           COUNT(*) FILTER (WHERE wl.status = 'approved')       AS approved
		    FROM teaching_courses tc
		    JOIN academic_terms trm ON trm.id = tc.term_id
		    JOIN submission_periods sp ON sp.term_id = tc.term_id
		    JOIN sections sec          ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		    JOIN users u ON u.id = a.ta_id
		    LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		    JOIN work_logs wl ON wl.assignment_id = a.id
		     AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		    LEFT JOIN submission_period_status st
		      ON st.submission_period_id = sp.id
		     AND st.ta_id = a.ta_id
		     AND st.teaching_course_id = tc.id
		    WHERE tc.term_id = $1
		      AND `+monthFilterSQL("wl.work_date", "$2")+`
		      -- Grad-special no longer logs work_logs at all (see
		      -- CourseExportBlockers) — leftover rows from before that change
		      -- must not permanently block the term-wide finance_sent gate.
		      AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		      AND CASE WHEN $3 = 'graduate' THEN a.level::text IN ('master','phd')
		               ELSE a.level::text = 'undergrad' END
		    GROUP BY 1, 2, 3, 4, 5, 6
		)
		SELECT course_code, ta_name, year_month, staff_status, waiting_ta, waiting_lecturer, approved
		FROM months
		WHERE waiting_ta > 0 OR waiting_lecturer > 0
		   OR (approved > 0 AND staff_status NOT IN ('exported','finance_sent'))
		ORDER BY course_code, sort_name, ta_name, year_month`, termID, months, level)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct{ kind, code, name string }
	agg := map[key]*ExportBlocker{}
	var order []key
	add := func(kind, code, name, ym string, n int) {
		k := key{kind, code, name}
		b, ok := agg[k]
		if !ok {
			b = &ExportBlocker{Kind: kind, TAName: name, CourseCode: code}
			agg[k] = b
			order = append(order, k)
		}
		b.Months = append(b.Months, ym)
		b.Rows += n
	}
	for rows.Next() {
		var code, name, ym, staffStatus string
		var waitingTA, waitingLecturer, approved int
		if err := rows.Scan(&code, &name, &ym, &staffStatus, &waitingTA, &waitingLecturer, &approved); err != nil {
			return nil, err
		}
		if waitingTA > 0 {
			add("waiting_ta", code, name, ym, waitingTA)
		}
		if waitingLecturer > 0 {
			add("waiting_lecturer", code, name, ym, waitingLecturer)
		}
		if waitingTA == 0 && waitingLecturer == 0 && approved > 0 &&
			staffStatus != "exported" && staffStatus != "finance_sent" {
			add("not_exported", code, name, ym, 0)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rank := map[string]int{"waiting_ta": 0, "waiting_lecturer": 1, "not_exported": 2}
	sort.SliceStable(order, func(i, j int) bool {
		if rank[order[i].kind] != rank[order[j].kind] {
			return rank[order[i].kind] < rank[order[j].kind]
		}
		if order[i].code != order[j].code {
			return order[i].code < order[j].code
		}
		return order[i].name < order[j].name
	})
	out := make([]ExportBlocker, 0, len(order))
	for _, k := range order {
		b := agg[k]
		b.Months = thaiMonthLabelsBE(b.Months)
		out = append(out, *b)
	}
	return out, nil
}

type classClashMonth struct {
	name, yearMonth string
	rows            int
}

// approvedClassClashes finds approved hours, in months not yet exported, that
// overlap the TA's CURRENT own-class timetable.
//
// The clash rule is checked when a row is logged and again at approval, but
// the TA edits their own timetable freely: delete a class, log hours over it,
// get them approved, put the class back. The approval-time recheck catches the
// class restored before approval; this catches it restored after — the last
// point before the hours become a claim document. Exported months are left
// alone: they are frozen, and the timetable edits are in the audit trail.
func (s *ExportService) approvedClassClashes(ctx context.Context, courseID uuid.UUID, months []string) ([]classClashMonth, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.ta_id, tc.term_id,
		       COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		       COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,''),
		       TO_CHAR(wl.work_date,'YYYY-MM'), TO_CHAR(wl.work_date,'YYYY-MM-DD'),
		       TO_CHAR(wl.start_time,'HH24:MI'), TO_CHAR(wl.end_time,'HH24:MI')
		FROM teaching_courses tc
		JOIN academic_terms trm ON trm.id = tc.term_id
		JOIN sections sec ON sec.teaching_course_id = tc.id
		JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		JOIN users u ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		JOIN work_logs wl ON wl.assignment_id = a.id AND wl.status = 'approved'
		JOIN submission_periods sp ON sp.term_id = tc.term_id
		 AND `+workLogInPeriodSQL("wl", "trm", "sp")+`
		LEFT JOIN submission_period_status st
		  ON st.submission_period_id = sp.id AND st.ta_id = a.ta_id AND st.teaching_course_id = tc.id
		WHERE tc.id = $1
		  AND `+monthFilterSQL("wl.work_date", "$2")+`
		  AND COALESCE(st.status,'pending') NOT IN ('exported','finance_sent')
		  AND (a.level::text NOT IN ('master','phd') OR sec.track <> 'special')
		ORDER BY u.first_name, wl.work_date, wl.start_time`, courseID, months)
	if err != nil {
		return nil, err
	}
	type logRow struct {
		ta, term                  uuid.UUID
		name, ym, day, start, end string
	}
	var logs []logRow
	for rows.Next() {
		var r logRow
		if err := rows.Scan(&r.ta, &r.term, &r.name, &r.ym, &r.day, &r.start, &r.end); err != nil {
			rows.Close()
			return nil, err
		}
		logs = append(logs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type blocksKey struct{ ta, term uuid.UUID }
	blocksOf := map[blocksKey][]ownClassBlock{}
	type monthKey struct{ name, ym string }
	counts := map[monthKey]int{}
	var order []monthKey
	for _, r := range logs {
		k := blocksKey{r.ta, r.term}
		blocks, ok := blocksOf[k]
		if !ok {
			if blocks, err = loadOwnClassBlocks(ctx, s.pool, r.ta, r.term); err != nil {
				return nil, err
			}
			blocksOf[k] = blocks
		}
		d, derr := timeutil.ParseDate(r.day)
		sm, ok1 := parseHM(r.start)
		em, ok2 := parseHM(r.end)
		if derr != nil || !ok1 || !ok2 || findOwnClassClash(blocks, int(d.Weekday()), sm, em) == nil {
			continue
		}
		mk := monthKey{r.name, r.ym}
		if counts[mk] == 0 {
			order = append(order, mk)
		}
		counts[mk]++
	}
	out := make([]classClashMonth, 0, len(order))
	for _, mk := range order {
		out = append(out, classClashMonth{name: mk.name, yearMonth: mk.ym, rows: counts[mk]})
	}
	return out, nil
}
