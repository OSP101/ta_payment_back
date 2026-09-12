package service

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// grad_special_schedule.go estimates how a grad-special TA's flat term lump
// (pay_rates.graduate_special_lumpsum, a whole-term-per-course figure) should
// be split across the term's calendar months.
//
// Per the 2026 staff meeting: grad-special TAs no longer log their own hours
// at all — the system computes their pay automatically. Asked what monthly
// split to use, the answer was "การคาดการภาคปกติ" — estimate it from the
// REGULAR track's own class schedule for the same course, since that's the
// only objective signal available once the TA isn't reporting anything
// themselves. A course taught mostly in December should show most of the
// special-track TA's money landing in December too, matching how the
// college's own example document (หลักฐาน-พิเศษ) splits one course's lump
// unevenly across its months (300 + 1,290 + 1,290, not a flat third each).

// gradSpecialMonthShares returns, for teaching course tcID, what fraction of
// the flat term lump belongs to each calendar month ("YYYY-MM"), weighted by
// the REGULAR-track section(s)' real scheduled class hours falling in that
// month (excluding full-day holidays and the midterm/final exam windows —
// those are not teaching days). Weights sum to 1.0 across every month that
// has any scheduled regular-track hours.
//
// Returns (nil, nil) when the course has no regular-track schedule to weight
// by (e.g. not filled in yet) — callers should fall back to an even split
// across the term's months in that case rather than fail outright.
func gradSpecialMonthShares(ctx context.Context, pool *pgxpool.Pool, tcID uuid.UUID) (map[string]float64, error) {
	var start, end time.Time
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(tc.starts_on, at.starts_on, '0001-01-01'::date),
		       COALESCE(tc.ends_on,   at.ends_on,   '9999-12-31'::date)
		FROM teaching_courses tc
		JOIN academic_terms at ON at.id = tc.term_id
		WHERE tc.id = $1`, tcID).Scan(&start, &end); err != nil {
		return nil, err
	}
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return nil, nil
	}

	var midStart, midEnd, finStart, finEnd *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT at.midterm_starts_on, at.midterm_ends_on, at.final_starts_on, at.final_ends_on
		FROM teaching_courses tc JOIN academic_terms at ON at.id = tc.term_id
		WHERE tc.id = $1`, tcID).Scan(&midStart, &midEnd, &finStart, &finEnd); err != nil {
		return nil, err
	}
	inWindow := func(d time.Time, s, e *time.Time) bool {
		return s != nil && e != nil && !d.Before(*s) && !d.After(*e)
	}

	holRows, err := pool.Query(ctx, `
		SELECT TO_CHAR(holiday_date,'YYYY-MM-DD')
		FROM public_holidays
		WHERE holiday_date BETWEEN $1::date AND $2::date
		  AND (start_time IS NULL OR end_time IS NULL)`, // full-day closures only
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	fullDayHoliday := map[string]bool{}
	for holRows.Next() {
		var d string
		if err := holRows.Scan(&d); err != nil {
			holRows.Close()
			return nil, err
		}
		fullDayHoliday[d] = true
	}
	holRows.Close()
	if err := holRows.Err(); err != nil {
		return nil, err
	}

	type weeklySlot struct {
		day   int
		hours float64
	}
	schRows, err := pool.Query(ctx, `
		SELECT ss.day_of_week, EXTRACT(EPOCH FROM (ss.end_time - ss.start_time)) / 3600
		FROM section_schedules ss
		JOIN sections sec ON sec.id = ss.section_id
		WHERE sec.teaching_course_id = $1 AND sec.track = 'regular'`, tcID)
	if err != nil {
		return nil, err
	}
	var slots []weeklySlot
	for schRows.Next() {
		var sl weeklySlot
		if err := schRows.Scan(&sl.day, &sl.hours); err != nil {
			schRows.Close()
			return nil, err
		}
		slots = append(slots, sl)
	}
	schRows.Close()
	if err := schRows.Err(); err != nil {
		return nil, err
	}
	if len(slots) == 0 {
		return nil, nil
	}

	byMonth := map[string]float64{}
	var total float64
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		if fullDayHoliday[ds] {
			continue
		}
		if inWindow(d, midStart, midEnd) || inWindow(d, finStart, finEnd) {
			continue
		}
		weekday := int(d.Weekday())
		for _, sl := range slots {
			if sl.day != weekday {
				continue
			}
			ym := d.Format("2006-01")
			byMonth[ym] += sl.hours
			total += sl.hours
		}
	}
	if total <= 0 {
		return nil, nil
	}
	out := make(map[string]float64, len(byMonth))
	for ym, hrs := range byMonth {
		out[ym] = hrs / total
	}
	return out, nil
}

// gradLumpByMonth places ONE graduate-special TA's flat term lump on the
// term's months (11/09/2026 rule): in proportion to the hours that TA logged
// on the course's special-track section(s) in each month. The lump is a
// whole-term figure but it is paid monthly, and the college wants each month
// to carry the share of the term that was actually worked in it. This split
// does not depend on the lecturer's settlement mode — it is not a shortfall
// being placed, it is a fixed sum being dated.
//
// approvedOnly selects which logs count: the documents and the settled
// figures read approved work only; the forecast reads everything not
// rejected, the same way the rest of the settlement does.
//
// A TA with no qualifying hours yet falls back to the course's regular-track
// schedule estimate (gradSpecialMonthShares, the 2026 meeting rule), and a
// course with no schedule to an even split over its term months — so the lump
// always lands somewhere and the slices always sum back to it.
//
// Each month's figure is a whole baht; what the rounding strands goes to the
// earliest month, exactly as spreadOverMonths does for hourly pay.
func (s *ExportService) gradLumpByMonth(
	ctx context.Context, courseID, taID uuid.UUID, lump float64, approvedOnly bool,
) (map[string]float64, error) {
	if lump <= 0 {
		return map[string]float64{}, nil
	}
	status := "w.status = 'approved'"
	if !approvedOnly {
		status = "w.status <> 'rejected'"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT to_char(w.work_date, 'YYYY-MM'), SUM(w.hours)
		FROM work_logs w
		JOIN ta_request_assignments a ON a.id = w.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		WHERE sec.teaching_course_id = $1 AND sec.track = 'special' AND a.ta_id = $2
		  AND `+status+`
		GROUP BY 1`, courseID, taID)
	if err != nil {
		return nil, err
	}
	weights := map[string]float64{}
	var total float64
	for rows.Next() {
		var ym string
		var hrs float64
		if err := rows.Scan(&ym, &hrs); err != nil {
			rows.Close()
			return nil, err
		}
		if hrs > 0 {
			weights[ym] = hrs
			total += hrs
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if total <= 0 {
		// Nothing logged yet: the regular-track schedule estimate.
		weights, err = gradSpecialMonthShares(ctx, s.pool, courseID)
		if err != nil {
			return nil, err
		}
	}
	if len(weights) == 0 {
		// No schedule either: evenly over the term's months.
		weights = map[string]float64{}
		all, err := s.CourseTermMonths(ctx, courseID)
		if err != nil {
			return nil, err
		}
		for _, m := range all {
			weights[m.YearMonth] = 1
		}
		if len(weights) == 0 {
			cal, err := courseCalendarMonths(ctx, s.pool, courseID)
			if err != nil {
				return nil, err
			}
			for _, ym := range cal {
				weights[ym] = 1
			}
		}
	}
	return placeLump(lump, weights), nil
}

// placeLump divides a lump over months by weight, whole baht each, remainder
// to the earliest month.
func placeLump(lump float64, weights map[string]float64) map[string]float64 {
	out := map[string]float64{}
	months := make([]string, 0, len(weights))
	var total float64
	for ym, w := range weights {
		if w > 0 {
			months = append(months, ym)
			total += w
		}
	}
	if len(months) == 0 || total <= 0 {
		return out
	}
	sort.Strings(months)
	placed := 0.0
	for _, ym := range months {
		out[ym] = math.Floor(lump * weights[ym] / total)
		placed += out[ym]
	}
	out[months[0]] += round2(lump - placed)
	return out
}

// sumMonths totals a per-month allocation over a selection; an empty
// selection means the whole thing.
func sumMonths(byMonth map[string]float64, months []string) float64 {
	if len(months) == 0 {
		var t float64
		for _, v := range byMonth {
			t += v
		}
		return t
	}
	var t float64
	for _, ym := range months {
		t += byMonth[ym]
	}
	return t
}
