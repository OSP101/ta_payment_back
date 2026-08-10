package service

import (
	"context"

	"github.com/google/uuid"
)

// billable_hours.go defines the ONE thing a TA is paid for: a sitting.
//
// Two sections taught in the same room at the same time are one sitting, not
// two. The printed claim form has always known this — buildClaimSheetRows
// merges overlapping and contiguous logs of the same activity into a single
// row, so "ปกติ Sec1-2  13.00 - 17.00" bills four hours whether it came from one
// section or three. Every screen and every budget figure, meanwhile, summed
// work_logs.hours raw, and therefore counted the overlap once per section.
//
// The two numbers disagreed by design and nobody could tell which was the
// claim: the export preview offered ฿5,120 for a TA whose signed claim form
// said ฿4,480. Settled 03/08/2026 in favour of the document — the form is what
// the college signs and the finance office pays against, so the screens now
// derive from the same rule.
//
// The rule lives here twice on purpose: once in SQL for the aggregate queries
// that span a whole term, once in Go (buildClaimSheetRows) for the row-by-row
// rendering of the form. billable_hours_test.go runs both over the same data and
// fails if they ever disagree, which is the only thing that makes two
// implementations safe.

// mergedSittingsCTE emits one row per merged sitting.
//
// The classic interval-merge: a log starts a new sitting when it begins after
// every earlier log in its group has ended; a running sum of those flags labels
// the sittings, and MIN/MAX collapse each label to its outer bounds. Chained
// overlaps (13-15, 14-16, 15-17) fold into one 13-17 sitting, exactly as the Go
// merger folds them.
//
// Grouped by (ta, course, track, date, activity, makeup) — the same key the form
// groups by. Different tracks never merge even at the same hour, because they
// are billed at different rates and printed on different sheets.
//
// Callers wrap this in their own WHERE by appending to `logFilter`.
// mergedSittingsWith builds the CTE for a chosen set of work_log statuses.
//
// The default (approved only) is what money is settled on. The forecast needs
// the same shape over everything still in play, so the filter is a parameter
// rather than a second copy of the interval-merge — one merge rule, two
// questions asked of it.
func mergedSittingsWith(statusFilter string) string {
	return `
	logs AS (
	    SELECT a.ta_id,
	           sec.teaching_course_id,
	           sec.track::text AS track,
	           a.level::text   AS level,
	           wl.work_date,
	           wl.activity,
	           (COALESCE(wl.note,'') LIKE '%ชดเชย%') AS makeup,
	           wl.start_time, wl.end_time
	    FROM work_logs wl
	    JOIN ta_request_assignments a ON a.id = wl.assignment_id
	    JOIN sections sec             ON sec.id = a.section_id
	    WHERE ` + statusFilter + `
	),
	marked AS (
	    SELECT *,
	           CASE WHEN start_time <= MAX(end_time) OVER (
	                    PARTITION BY ta_id, teaching_course_id, track, work_date, activity, makeup
	                    ORDER BY start_time, end_time
	                    ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING)
	                THEN 0 ELSE 1 END AS starts_new
	    FROM logs
	),
	grouped AS (
	    SELECT *,
	           SUM(starts_new) OVER (
	               PARTITION BY ta_id, teaching_course_id, track, work_date, activity, makeup
	               ORDER BY start_time, end_time
	               ROWS UNBOUNDED PRECEDING) AS sitting
	    FROM marked
	),
	sittings AS (
	    SELECT ta_id, teaching_course_id, track, level, work_date,
	           MIN(start_time) AS start_time, MAX(end_time) AS end_time,
	           EXTRACT(EPOCH FROM (MAX(end_time) - MIN(start_time))) / 3600.0 AS hours
	    FROM grouped
	    GROUP BY ta_id, teaching_course_id, track, level, work_date, activity, makeup, sitting
	)`
}

// mergedSittingsCTE is the settled view: approved work only.
var mergedSittingsCTE = mergedSittingsWith("wl.status = 'approved'")

// mergedSittingsForecastCTE is everything still in play — approved, submitted
// and draft. A rejected row is excluded: it is not work anybody intends to pay
// for unless the TA fixes and resends it.
var mergedSittingsForecastCTE = mergedSittingsWith("wl.status <> 'rejected'")

// billableHoursGo is the Go side of the same rule, expressed through the code
// the form itself uses. Kept here so the cross-check test has one call to make
// rather than reassembling the pipeline.
func billableHoursGo(rows []claimLogRow) float64 {
	byTrack := map[string][]claimLogRow{}
	for _, r := range rows {
		byTrack[r.Track] = append(byTrack[r.Track], r)
	}
	var total float64
	for track, list := range byTrack {
		word := "ปกติ"
		if track == "special" {
			word = "พิเศษ"
		}
		for _, printed := range buildClaimSheetRows(list, word) {
			total += claimHours(printed.Range)
		}
	}
	return total
}

// taTrackKey is the pair money is billed at: a person and a programme track.
type taTrackKey struct {
	TA    uuid.UUID
	Track string
}

// billableHoursByTATrack totals a course's sittings per (TA, track).
//
// The key is deliberately NOT the assignment: a sitting can span two sections,
// so it belongs to the pair the money is billed at, not to one of the sections
// that happened to be in it.
// months (Gregorian "YYYY-MM", empty = the whole term) restricts the count to
// one fiscal slice — see monthFilterSQL.
func (s *ExportService) billableHoursByTATrack(ctx context.Context, courseID uuid.UUID, months []string) (map[taTrackKey]float64, error) {
	rows, err := s.pool.Query(ctx, `WITH`+mergedSittingsCTE+`
		SELECT ta_id, track, SUM(hours)
		FROM sittings
		WHERE teaching_course_id = $1
		  AND `+monthFilterSQL("work_date", "$2")+`
		GROUP BY ta_id, track`, courseID, months)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[taTrackKey]float64{}
	for rows.Next() {
		var ta uuid.UUID
		var track string
		var hours float64
		if err := rows.Scan(&ta, &track, &hours); err != nil {
			return nil, err
		}
		out[taTrackKey{ta, track}] = hours
	}
	return out, rows.Err()
}
