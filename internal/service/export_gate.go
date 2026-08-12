package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// ExportBlocker is one reason a course may not be exported yet, phrased for the
// staff screen rather than for a log.
type ExportBlocker struct {
	// Kind is "waiting_ta" | "waiting_lecturer" | "unreviewed" | "not_finance_sent".
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
		           COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'') AS ta_name,
		           sp.year_month,
		           COALESCE(st.status,'pending') AS staff_status,
		           COUNT(*) FILTER (WHERE `+waitingTASQL("wl")+`)       AS waiting_ta,
		           COUNT(*) FILTER (WHERE `+waitingLecturerSQL("wl")+`) AS waiting_lecturer,
		           COUNT(*) FILTER (WHERE wl.status = 'approved')       AS approved
		    FROM teaching_courses tc
		    JOIN submission_periods sp ON sp.term_id = tc.term_id
		    JOIN sections sec          ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		    JOIN users u ON u.id = a.ta_id
		    JOIN work_logs wl ON wl.assignment_id = a.id
		     AND to_char(wl.work_date,'MM') = RIGHT(sp.year_month, 2)
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
		    GROUP BY 1, 2, 3, 4
		)
		SELECT ta_name, year_month, staff_status, waiting_ta, waiting_lecturer, approved
		FROM months
		WHERE waiting_ta > 0 OR waiting_lecturer > 0
		   OR (approved > 0 AND staff_status NOT IN ('staff_reviewed','exported','finance_sent'))
		ORDER BY ta_name, year_month`, courseID, months)
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
		if err := rows.Scan(&name, &ym, &staffStatus, &waitingTA, &waitingLecturer, &approved); err != nil {
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
			add("unreviewed", name, ym, 0)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Stage order, then name — staff read this list top-down as a work queue.
	rank := map[string]int{"waiting_ta": 0, "waiting_lecturer": 1, "unreviewed": 2}
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
		case "not_finance_sent":
			lines = append(lines, fmt.Sprintf("%s ตรวจสอบแล้วแต่ยังไม่ส่งการเงิน (%s)", name, months))
		default:
			lines = append(lines, fmt.Sprintf("%s ยังไม่ได้ตรวจสอบเบิกจ่าย (%s)", name, months))
		}
	}
	return Invalid("ยังส่งออกไม่ได้ ต้องผ่านครบทุกขั้นก่อน:\n• " + strings.Join(lines, "\n• "))
}

// TermExportBlockers is the ประตู before generating ปะหน้าจ่ายตรง
// (transfer-cover): every course in the term must have reached finance_sent,
// not merely staff_reviewed — this document IS the finance notice, so a
// course that is reviewed but not yet sent to finance is still blocking.
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
// October is still being taught and could not possibly have reached
// finance_sent. A term-wide gate makes that document unobtainable until after
// the budget year it belongs to has already closed.
//
// Filtered on work_date's own Gregorian month, not on sp.year_month, which
// carries a BUDDHIST academic year ("2569-06") and cannot be compared with a
// caller's Gregorian selection — see term_months.go.
func (s *ExportService) TermExportBlockers(ctx context.Context, termID uuid.UUID, months []string) ([]ExportBlocker, error) {
	rows, err := s.pool.Query(ctx, `
		WITH months AS (
		    SELECT tc.code AS course_code,
		           a.ta_id,
		           COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'') AS ta_name,
		           sp.year_month,
		           COALESCE(st.status,'pending') AS staff_status,
		           COUNT(*) FILTER (WHERE `+waitingTASQL("wl")+`)       AS waiting_ta,
		           COUNT(*) FILTER (WHERE `+waitingLecturerSQL("wl")+`) AS waiting_lecturer,
		           COUNT(*) FILTER (WHERE wl.status = 'approved')       AS approved
		    FROM teaching_courses tc
		    JOIN submission_periods sp ON sp.term_id = tc.term_id
		    JOIN sections sec          ON sec.teaching_course_id = tc.id
		    JOIN ta_request_assignments a ON a.section_id = sec.id AND a.state <> 'dropped'
		    JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		    JOIN users u ON u.id = a.ta_id
		    JOIN work_logs wl ON wl.assignment_id = a.id
		     AND to_char(wl.work_date,'MM') = RIGHT(sp.year_month, 2)
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
		    GROUP BY 1, 2, 3, 4, 5
		)
		SELECT course_code, ta_name, year_month, staff_status, waiting_ta, waiting_lecturer, approved
		FROM months
		WHERE waiting_ta > 0 OR waiting_lecturer > 0
		   OR (approved > 0 AND staff_status <> 'finance_sent')
		ORDER BY course_code, ta_name, year_month`, termID, months)
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
		if waitingTA == 0 && waitingLecturer == 0 && approved > 0 && staffStatus != "finance_sent" {
			add("not_finance_sent", code, name, ym, 0)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rank := map[string]int{"waiting_ta": 0, "waiting_lecturer": 1, "not_finance_sent": 2}
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
