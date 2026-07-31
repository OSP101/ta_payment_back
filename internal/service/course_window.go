package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A teaching course's effective date range, and the count of class periods that
// fall on a holiday with no makeup filed yet.
//
// # WHY THIS FILE EXISTS
//
// The same formula was written out three times — TeachingService.Get,
// TeachingService.List and HolidayService.ImpactsForCourse — each with a comment
// claiming it matched the others. Two of them fell back to CURRENT_DATE when the
// course had no dates of its own, and one fell back to the academic term's dates.
//
// Every course in the registrar import leaves teaching_courses.starts_on and
// ends_on NULL (127 of 127 here), so that fallback decided the answer for every
// course in the system:
//
//	Get()               → term window  → the sidebar badge said "8"
//	ImpactsForCourse()  → today..today → the page it links to said "no holidays"
//
// A one-day window ending where it starts is not a course; it is an accident of
// COALESCE reaching for the nearest non-null value. The term window is the real
// one, and Get's own comment already said so ("the UI treats these as the
// canonical range for the course").
//
// The lesson is the reason these are functions and not three copies: a comment
// saying "same as X" is not enforcement. Only a shared definition is.

// CourseStartSQL / CourseEndSQL are the course's effective term boundaries: its
// own dates when set, otherwise the parent academic term's.
//
// Written as correlated subqueries rather than requiring a JOIN so they drop into
// any query that already has the teaching_courses alias, without forcing callers
// to restructure their FROM clause (which is how the copies diverged).
func CourseStartSQL(tcAlias string) string {
	return `COALESCE(` + tcAlias + `.starts_on,
		(SELECT at_w.starts_on FROM academic_terms at_w WHERE at_w.id = ` + tcAlias + `.term_id))`
}

func CourseEndSQL(tcAlias string) string {
	return `COALESCE(` + tcAlias + `.ends_on,
		(SELECT at_w.ends_on FROM academic_terms at_w WHERE at_w.id = ` + tcAlias + `.term_id))`
}

// UnresolvedMakeupsSQL counts (holiday × section × scheduled period) rows inside
// the course window that have no makeup_schedules entry.
//
// It counts PERIODS, not holiday dates: one public holiday landing on a day when
// three sections each hold a lecture is three periods a lecturer must reschedule,
// and three periods a TA cannot log hours against. The holidays page groups them
// by date for display, so the badge and the page can legitimately show different
// numbers — 8 periods across 5 dates — but they must never disagree about whether
// there is anything to do at all.
func UnresolvedMakeupsSQL(tcAlias string) string {
	return `(SELECT COUNT(*)
		   FROM public_holidays h
		   JOIN sections sx ON sx.teaching_course_id = ` + tcAlias + `.id
		   JOIN section_schedules sch ON sch.section_id = sx.id
		        AND sch.day_of_week = EXTRACT(DOW FROM h.holiday_date)::int
		        -- Partial-day holidays count only the periods they overlap. This
		        -- predicate is duplicated in HolidayService.ImpactsForCourse and
		        -- must stay identical: the badge and the page it links to are the
		        -- pair that has already diverged once (see the header comment).
		        AND (h.start_time IS NULL
		             OR (sch.start_time < h.end_time AND sch.end_time > h.start_time))
		   -- Matched on kind as well: a makeup replaces ONE period. Joining on
		   -- (section, date) alone counted the lab's makeup as covering the
		   -- lecture too, so a half-resolved day reported as fully resolved.
		   LEFT JOIN makeup_schedules m ON m.section_id = sx.id
		        AND m.original_date = h.holiday_date
		        AND m.kind = sch.kind
		  WHERE m.id IS NULL
		    AND h.holiday_date BETWEEN ` + CourseStartSQL(tcAlias) + `
		                           AND ` + CourseEndSQL(tcAlias) + `)`
}

// WeeksInTerm is how many weeks of classes a course actually runs, from its
// effective term dates (CourseStartSQL / CourseEndSQL), counting both endpoints.
//
// It replaces `months × 4`, which was wrong in a way that cost TAs money. Term
// 2569/1 runs 22 Jun – 18 Oct: 119 days, exactly 17 weeks. The old formula called
// it 4 × 4 = 16, so the term-hour ceiling came out at 96 while the timetable the
// lecturer set produces 98 — every TA on the course was over the ceiling before
// they logged anything by hand. The calendar is not negotiable and the
// approximation was; this uses the calendar.
//
// Falls back to months × 4 only when a course has no dates at all on either
// itself or its term, which cannot happen for an imported course but would
// otherwise return zero and remove the ceiling entirely.
func WeeksInTerm(ctx context.Context, pool *pgxpool.Pool, teachingCourseID uuid.UUID) float64 {
	var days *int
	var months int
	_ = pool.QueryRow(ctx, `
		SELECT (`+CourseEndSQL("tc")+` - `+CourseStartSQL("tc")+`) + 1,
		       COALESCE(NULLIF(at.months, 0), 4)
		  FROM teaching_courses tc
		  JOIN academic_terms at ON at.id = tc.term_id
		 WHERE tc.id = $1`, teachingCourseID).Scan(&days, &months)
	if days != nil && *days > 0 {
		return float64(*days) / 7.0
	}
	if months <= 0 {
		months = 4
	}
	return float64(months) * 4.0
}
