// term_months.go resolves "which months does this term have, and what is each
// one called" — the shared vocabulary the fiscal-year document split speaks in.
//
// Two calendars meet here and must never be confused:
//
//   - submission_periods.year_month is "<BUDDHIST academic year>-<MM>", e.g.
//     "2569-06". The year is the ACADEMIC year, not the calendar year of that
//     month, so it cannot be compared against a date column directly.
//   - work_logs.work_date — and therefore SlotSettlement.YearMonth, which every
//     money figure is keyed by — is an ordinary Gregorian date, "2026-06".
//
// Everything downstream (the gate, the builder, the ledger, the API) uses the
// GREGORIAN key. This file is the only place the conversion happens.
package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// monthFilterSQL renders the predicate that narrows a query to a month
// selection. dateCol is a DATE column, param the placeholder holding a
// text[] of Gregorian "YYYY-MM" keys.
//
// COALESCE, not a bare cardinality(): a nil Go slice arrives as SQL NULL and
// cardinality(NULL) is NULL, not 0, which would make the whole predicate NULL
// and silently drop every row — a whole-term export that quietly contains
// nothing, or a gate that reports clean because it examined no rows. One
// helper so that trap is fixed in one place rather than re-set at each call.
func monthFilterSQL(dateCol, param string) string {
	return "(COALESCE(cardinality(" + param + "::text[]), 0) = 0" +
		" OR to_char(" + dateCol + ",'YYYY-MM') = ANY(" + param + "::text[]))"
}

// TermMonth is one claimable month of a term, in both calendars plus the Thai
// label staff already read on the submission-period screens.
type TermMonth struct {
	// YearMonth is the Gregorian key, "2026-06" — what money is keyed by.
	YearMonth string `json:"year_month"`
	// PeriodYearMonth is submission_periods' own Buddhist key, "2569-06",
	// carried so callers can join back to that table without re-deriving it.
	PeriodYearMonth string `json:"period_year_month"`
	Label           string `json:"label"` // "มิถุนายน 2569"
}

// gregorianYearMonth converts a submission_periods.year_month ("2569-06") to
// the Gregorian key work_date produces ("2026-06").
//
// A Thai academic year Y (Buddhist) opens in June of Gregorian Y-543 and runs
// to the following May, so June–December belong to Y-543 and January–May to
// Y-543+1. That is exactly the wrap BulkCreateForTerm applies when it stamps
// starts_on for the second semester's มกราคม–มีนาคม periods.
//
// Deliberately derived rather than read from submission_periods.starts_on:
// that column is pulled BACKWARDS for any month whose shared due date falls
// before it (ประกาศ has มิ.ย.–ส.ค. all closing 31 ก.ค.), so August's starts_on
// reads 2026-07-01 and would place August's money in July.
func gregorianYearMonth(periodYearMonth string) (string, error) {
	y, m, ok := strings.Cut(periodYearMonth, "-")
	if !ok {
		return "", fmt.Errorf("year_month %q: want <year>-<MM>", periodYearMonth)
	}
	year, err := strconv.Atoi(y)
	if err != nil {
		return "", fmt.Errorf("year_month %q: %w", periodYearMonth, err)
	}
	month, err := strconv.Atoi(m)
	if err != nil {
		return "", fmt.Errorf("year_month %q: %w", periodYearMonth, err)
	}
	if month < 1 || month > 12 {
		return "", fmt.Errorf("year_month %q: month out of range", periodYearMonth)
	}
	greg := year - 543
	if month <= 5 {
		greg++
	}
	return fmt.Sprintf("%04d-%02d", greg, month), nil
}

// TermMonths lists a term's claimable months in calendar order. Sourced from
// submission_periods because that is what staff created and named; a term with
// none yet returns an empty list, and callers treat that as "no month scoping
// possible" rather than "no months exist".
func (s *ExportService) TermMonths(ctx context.Context, termID uuid.UUID) ([]TermMonth, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT year_month, label
		FROM submission_periods
		WHERE term_id = $1
		ORDER BY year_month`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TermMonth
	for rows.Next() {
		var tm TermMonth
		if err := rows.Scan(&tm.PeriodYearMonth, &tm.Label); err != nil {
			return nil, err
		}
		greg, err := gregorianYearMonth(tm.PeriodYearMonth)
		if err != nil {
			return nil, err
		}
		tm.YearMonth = greg
		out = append(out, tm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// ORDER BY year_month sorts the BUDDHIST key, which puts มกราคม before
	// มิถุนายน for a second semester that runs พ.ย.→มี.ค. Re-sort on the
	// Gregorian key so "in calendar order" is true for every term shape.
	sortTermMonths(out)
	return out, nil
}

func sortTermMonths(in []TermMonth) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j].YearMonth < in[j-1].YearMonth; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// normalizeMonthSelection validates a caller's requested months against the
// term and returns them in calendar order, de-duplicated.
//
// An empty request means the WHOLE term — the behaviour every caller had
// before the fiscal split existed, so an old client that sends no months keeps
// producing exactly the document it produced before. A request naming a month
// the term does not have is an error rather than a silent drop: quietly
// exporting fewer months than asked for is how money goes missing.
func normalizeMonthSelection(all []TermMonth, requested []string) ([]string, error) {
	valid := map[string]bool{}
	for _, m := range all {
		valid[m.YearMonth] = true
	}
	if len(requested) == 0 {
		out := make([]string, 0, len(all))
		for _, m := range all {
			out = append(out, m.YearMonth)
		}
		return out, nil
	}
	seen := map[string]bool{}
	for _, r := range requested {
		if !valid[r] {
			return nil, fmt.Errorf("เดือน %s ไม่อยู่ในภาคเรียนนี้", r)
		}
		seen[r] = true
	}
	out := make([]string, 0, len(seen))
	for _, m := range all {
		if seen[m.YearMonth] {
			out = append(out, m.YearMonth)
		}
	}
	return out, nil
}

// ResolveCourseMonths turns a caller's month selection into the explicit list
// the export actually covers — an empty selection becoming every month of the
// course's term.
//
// (13/08/2026) This exists because "no selection" and "every month" were the
// same value (nil) all the way from the query string into export_batches.months,
// where NULL is stored. Two readers then disagreed about what that NULL meant:
// CourseExportCoverage treats it as covering everything (below), while the
// round predicates test `months && $1`, and `NULL && anything` is NULL, not
// true. So a whole-term ZIP that really did claim ตุลาคม left the course flagged
// "รอบ 2 ค้าง" forever, and stuck on the round-2 progress board with it.
// Resolving here means the ledger records what was in the file, and NULL goes
// back to meaning only what the migration says it means: a pre-split row.
func (s *ExportService) ResolveCourseMonths(
	ctx context.Context, courseID uuid.UUID, requested []string,
) ([]string, error) {
	all, err := s.CourseTermMonths(ctx, courseID)
	if err != nil {
		return nil, err
	}
	return normalizeMonthSelection(all, requested)
}

// CourseExportCoverage answers "which months of this course have already been
// claimed, and which have not" — the question that decides whether staff are
// about to bill ตุลาคม twice or never bill it at all. Free month selection
// makes both mistakes possible, so it is read off the export ledger rather
// than left to memory.
type CourseExportCoverage struct {
	Months []TransferCoverMonthStatus `json:"months"`
	Split  FiscalSplit                `json:"fiscal_split"`
}

func (s *ExportService) CourseExportCoverage(ctx context.Context, courseID uuid.UUID) (*CourseExportCoverage, error) {
	all, err := s.CourseTermMonths(ctx, courseID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT COALESCE(months, '{}') FROM export_batches WHERE teaching_course_id = $1`, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issued := map[string]bool{}
	for rows.Next() {
		var ms []string
		if err := rows.Scan(&ms); err != nil {
			return nil, err
		}
		if len(ms) == 0 {
			// A batch recorded before the split existed covered everything.
			for _, m := range all {
				issued[m.YearMonth] = true
			}
			continue
		}
		for _, m := range ms {
			issued[m] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	split, err := fiscalSplit(all)
	if err != nil {
		return nil, err
	}
	out := &CourseExportCoverage{Split: split, Months: make([]TransferCoverMonthStatus, 0, len(all))}
	for _, m := range all {
		out.Months = append(out.Months, TransferCoverMonthStatus{TermMonth: m, Issued: issued[m.YearMonth]})
	}
	return out, nil
}

// fiscalYearOf returns the Thai budget year a Gregorian month falls in. The
// budget year runs 1 October → 30 September and is named for the year it ENDS
// in, so October 2026 already belongs to the year that closes in 2027.
func fiscalYearOf(yearMonth string) (int, error) {
	y, m, ok := strings.Cut(yearMonth, "-")
	if !ok {
		return 0, fmt.Errorf("year_month %q: want <year>-<MM>", yearMonth)
	}
	year, err := strconv.Atoi(y)
	if err != nil {
		return 0, fmt.Errorf("year_month %q: %w", yearMonth, err)
	}
	month, err := strconv.Atoi(m)
	if err != nil {
		return 0, fmt.Errorf("year_month %q: %w", yearMonth, err)
	}
	if month >= 10 {
		year++
	}
	return year, nil
}

// CourseTermMonths lists the months of the term a course belongs to.
func (s *ExportService) CourseTermMonths(ctx context.Context, courseID uuid.UUID) ([]TermMonth, error) {
	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT term_id FROM teaching_courses WHERE id = $1`, courseID).Scan(&termID); err != nil {
		return nil, err
	}
	return s.TermMonths(ctx, termID)
}

// courseMonthShare returns what fraction of the course's term a month
// selection covers, plus a membership test for filtering คาบ. The fraction
// apportions figures that are flat per term and have no คาบ behind them — the
// graduate-special lump — so that slicing a term still sums to the whole.
//
// An empty selection means the whole term: share 1, everything in slice.
func (s *ExportService) courseMonthShare(
	ctx context.Context, courseID uuid.UUID, months []string,
) (float64, func(string) bool, error) {
	if len(months) == 0 {
		return 1, func(string) bool { return true }, nil
	}
	selected := map[string]bool{}
	for _, m := range months {
		selected[m] = true
	}
	in := func(ym string) bool { return selected[ym] }
	all, err := s.CourseTermMonths(ctx, courseID)
	if err != nil {
		return 0, nil, err
	}
	if len(all) == 0 {
		return 1, in, nil
	}
	hit := 0
	for _, m := range all {
		if selected[m.YearMonth] {
			hit++
		}
	}
	return float64(hit) / float64(len(all)), in, nil
}

// FiscalSplit reports where the Thai budget year cuts this term, if it does.
//
// The budget year ends 30 September, so a term teaching across that boundary
// must be claimed on two documents: months up to September against the closing
// year's appropriation, October onward against the new one. Returned as a
// suggestion for the export screen to preselect — never enforced, because staff
// may have their own reason to slice differently.
//
// Only ภาคต้น (มิ.ย.–ต.ค.) actually crosses. ภาคปลาย runs พ.ย.→มี.ค., which sits
// entirely inside ONE budget year despite spanning two calendar years — hence
// grouping by budget year rather than by "is the month October or later".
type FiscalSplit struct {
	Crosses bool     `json:"crosses"`
	Before  []string `json:"before"` // the earlier budget year (closes 30 ก.ย.)
	After   []string `json:"after"`  // the later budget year (opens 1 ต.ค.)
}

func fiscalSplit(all []TermMonth) (FiscalSplit, error) {
	if len(all) == 0 {
		return FiscalSplit{}, nil
	}
	first, err := fiscalYearOf(all[0].YearMonth)
	if err != nil {
		return FiscalSplit{}, err
	}
	var before, after []string
	for _, m := range all {
		fy, err := fiscalYearOf(m.YearMonth)
		if err != nil {
			return FiscalSplit{}, err
		}
		if fy == first {
			before = append(before, m.YearMonth)
		} else {
			after = append(after, m.YearMonth)
		}
	}
	return FiscalSplit{Crosses: len(after) > 0, Before: before, After: after}, nil
}
