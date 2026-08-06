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
	// Kind is "waiting_ta" | "waiting_lecturer" | "unreviewed".
	Kind   string `json:"kind"`
	TAName string `json:"ta_name"`
	// Months affected, as Thai labels ("สิงหาคม 2569").
	Months []string `json:"months"`
	Rows   int      `json:"rows,omitempty"`
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
func (s *ExportService) CourseExportBlockers(ctx context.Context, courseID uuid.UUID) ([]ExportBlocker, error) {
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
		    GROUP BY 1, 2, 3, 4
		)
		SELECT ta_name, year_month, staff_status, waiting_ta, waiting_lecturer, approved
		FROM months
		WHERE waiting_ta > 0 OR waiting_lecturer > 0
		   OR (approved > 0 AND staff_status NOT IN ('staff_reviewed','exported','finance_sent'))
		ORDER BY ta_name, year_month`, courseID)
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
		switch b.Kind {
		case "waiting_ta":
			lines = append(lines, fmt.Sprintf("%s ยังไม่ส่งบันทึกเวลา %d รายการ (%s)", b.TAName, b.Rows, months))
		case "waiting_lecturer":
			lines = append(lines, fmt.Sprintf("%s รออาจารย์อนุมัติ %d รายการ (%s)", b.TAName, b.Rows, months))
		default:
			lines = append(lines, fmt.Sprintf("%s ยังไม่ได้ตรวจสอบเบิกจ่าย (%s)", b.TAName, months))
		}
	}
	return Invalid("ยังส่งออกไม่ได้ ต้องผ่านครบทุกขั้นก่อน:\n• " + strings.Join(lines, "\n• "))
}
