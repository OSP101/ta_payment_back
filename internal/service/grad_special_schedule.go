package service

import (
	"context"
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
