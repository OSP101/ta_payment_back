package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ProgressScopeValid accepts historical folders or a Gregorian monthly scope.
func ProgressScopeValid(scope int) bool {
	return scope == 1 || scope == 2 || (scope >= 190001 && scope <= 999912 && scope%100 >= 1 && scope%100 <= 12)
}
func monthlyScope(ym string) int  { n, _ := strconv.Atoi(strings.ReplaceAll(ym, "-", "")); return n }
func scopeMonth(scope int) string { return fmt.Sprintf("%04d-%02d", scope/100, scope%100) }

func (s *ExportService) LegacyPayoutMonths(ctx context.Context, termID uuid.UUID) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT year_month FROM payout_legacy_months WHERE term_id=$1`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var ym string
		if err = rows.Scan(&ym); err != nil {
			return nil, err
		}
		out[ym] = true
	}
	return out, rows.Err()
}

// ValidatePayoutSelection permits historical slices only when every selected
// month belongs to an old folder. Every new claim must name exactly one month.
func (s *ExportService) ValidatePayoutSelection(ctx context.Context, termID uuid.UUID, months []string) error {
	legacy, err := s.LegacyPayoutMonths(ctx, termID)
	if err != nil {
		return err
	}
	if len(months) == 0 {
		return Invalid("กรุณาเลือกเดือนที่จะเบิกหนึ่งเดือน")
	}
	if len(months) > 1 {
		for _, ym := range months {
			if !legacy[ym] {
				return Invalid("ชุดเบิกใหม่ต้องส่งออกครั้งละหนึ่งเดือนเท่านั้น")
			}
		}
	}
	return nil
}

// MonthlyPayoutClosed checks the actual staff deadline, including explicitly
// closed periods. Kept identical to submission rules so an open month cannot
// be frozen while its TAs may still submit.
func (s *ExportService) MonthlyPayoutClosed(ctx context.Context, termID uuid.UUID, ym string) (bool, error) {
	var closed bool
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(bool_and(`+periodClosedSQL("sp")+`),false)
 FROM submission_periods sp WHERE sp.term_id=$1 AND
 ((left(sp.year_month,4)::int-543+CASE WHEN right(sp.year_month,2)::int<=5 THEN 1 ELSE 0 END)::text||'-'||right(sp.year_month,2))=$2`, termID, ym).Scan(&closed)
	return closed, err
}

// PayoutMonthCourses excludes hourly TAs with no approved claim. A graduate
// special-track assignment is payable even without a submitted work log.
func (s *ExportService) PayoutMonthCourses(ctx context.Context, termID uuid.UUID, ym string) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT tc.id FROM teaching_courses tc WHERE tc.term_id=$1 AND `+roundBillableSQL("$2")+` ORDER BY tc.code`, termID, []string{ym})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *DocumentProgressService) monthlyOverview(ctx context.Context, termID uuid.UUID) (*TermProgressOverview, error) {
	all, err := s.export.TermMonths(ctx, termID)
	if err != nil {
		return nil, err
	}
	legacy, err := s.export.LegacyPayoutMonths(ctx, termID)
	if err != nil {
		return nil, err
	}
	out := &TermProgressOverview{TermID: termID, Rounds: []TermProgress{}}
	if len(legacy) > 0 {
		fr, e := s.resolveFiscalRounds(ctx, termID)
		if e != nil {
			return nil, e
		}
		for _, r := range []int{1, 2} {
			if r == 2 && !fr.twoRounds {
				continue
			}
			p, e := s.getRoundProgress(ctx, termID, r, fr)
			if e != nil {
				return nil, e
			}
			p.Legacy = true
			p.RoundLabel = "เอกสารเดิม · " + p.RoundLabel
			out.Rounds = append(out.Rounds, *p)
		}
	}
	for _, m := range all {
		if legacy[m.YearMonth] {
			continue
		}
		p, e := s.getRoundProgress(ctx, termID, monthlyScope(m.YearMonth), &fiscalRounds{})
		if e != nil {
			return nil, e
		}
		p.RoundLabel = m.Label
		p.PeriodClosed, e = s.export.MonthlyPayoutClosed(ctx, termID, m.YearMonth)
		if e != nil {
			return nil, e
		}
		if e = s.pool.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM monthly_payout_batches WHERE term_id=$1 AND year_month=$2`, termID, m.YearMonth).Scan(&p.IssuedVersion); e != nil {
			return nil, e
		}
		// Signing is enabled only after the combined month pack was committed,
		// never merely after one course's ZIP was downloaded.
		var issued bool
		e = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM monthly_payout_batches WHERE term_id=$1 AND year_month=$2)`, termID, m.YearMonth).Scan(&issued)
		if e != nil {
			return nil, e
		}
		p.AllExported = p.AllExported && issued
		p.CanAdvance = p.CanAdvance && p.AllExported
		out.Rounds = append(out.Rounds, *p)
	}
	return out, nil
}

func monthlyCourseExportedSQL(param string) string {
	return "(" + roundExportedSQL(param) + ` AND NOT EXISTS (
 SELECT 1 FROM work_logs wl JOIN ta_request_assignments a ON a.id=wl.assignment_id
 JOIN sections sec ON sec.id=a.section_id
 JOIN academic_terms trm ON trm.id=tc.term_id
 JOIN submission_periods sp ON sp.term_id=tc.term_id AND sp.year_month=trm.academic_year::text||'-'||to_char(wl.work_date,'MM')
 LEFT JOIN submission_period_status st ON st.submission_period_id=sp.id AND st.teaching_course_id=tc.id AND st.ta_id=a.ta_id
 WHERE sec.teaching_course_id=tc.id AND wl.status='approved'
 AND (a.level::text NOT IN ('master','phd') OR sec.track<>'special')
 AND to_char(wl.work_date,'YYYY-MM')=ANY(` + param + `::text[])
 AND COALESCE(st.status,'pending') NOT IN ('exported','finance_sent')))`
}
func scopedCourseExportedSQL(scope int, param string) string {
	if scope > 2 {
		return monthlyCourseExportedSQL(param)
	}
	return roundExportedSQL(param)
}

// A later assignment or a new lecturer must not change who signed an issued
// document. The original roster is retained on each monthly batch version.
func (s *DocumentProgressService) monthlySignerSnapshot(ctx context.Context, termID uuid.UUID, scope int) ([]SignatureItem, bool, error) {
	var batchID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM monthly_payout_batches WHERE term_id=$1 AND year_month=$2 ORDER BY version DESC LIMIT 1`, termID, scopeMonth(scope)).Scan(&batchID)
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := s.pool.Query(ctx, `SELECT p.teaching_course_id,p.code,p.name_th,p.role,p.signer_id,p.responsible,`+monthlyCourseExportedSQL("$4")+`,to_char(sc.signed_at,'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
 FROM monthly_payout_signers p JOIN teaching_courses tc ON tc.id=p.teaching_course_id LEFT JOIN signature_checklist sc ON sc.term_id=$1 AND sc.fiscal_round=$2 AND sc.teaching_course_id=p.teaching_course_id AND sc.role=p.role AND sc.signer_id IS NOT DISTINCT FROM p.signer_id
 WHERE p.batch_id=$3 ORDER BY p.code,p.role,p.responsible`, termID, scope, batchID, []string{scopeMonth(scope)})
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []SignatureItem{}
	labels := map[string]string{}
	for _, r := range signatureRoles {
		labels[r.key] = r.label
	}
	for rows.Next() {
		var it SignatureItem
		if err = rows.Scan(&it.TeachingCourseID, &it.Code, &it.NameTH, &it.Role, &it.SignerID, &it.Responsible, &it.Exported, &it.SignedAt); err != nil {
			return nil, false, err
		}
		it.RoleLabel = labels[it.Role]
		out = append(out, it)
	}
	return out, true, rows.Err()
}

func (s *DocumentProgressService) validateLegacyMutation(ctx context.Context, termID uuid.UUID, scope int) error {
	if scope > 2 || s.export == nil {
		return nil
	}
	legacy, e := s.export.LegacyPayoutMonths(ctx, termID)
	if e != nil {
		return e
	}
	if len(legacy) == 0 {
		return Invalid("ภาคเรียนนี้ใช้ชุดเบิกรายเดือน กรุณาเลือกเดือน")
	}
	return nil
}

// MonthlyArchiveCurrent reads the original scope rather than today's roster.
// A changed roster/rate does not invalidate a printed document. Explicitly
// reopening one of its paid cells or issuing a replacement course file does.
func (s *ExportService) MonthlyArchiveCurrent(ctx context.Context, ym string, issuedAt time.Time, courses []uuid.UUID) (bool, error) {
	var current bool
	err := s.pool.QueryRow(ctx, `SELECT NOT EXISTS(
 SELECT 1 FROM export_month_ledger l
 JOIN teaching_courses tc ON tc.id=l.teaching_course_id
 JOIN academic_terms trm ON trm.id=tc.term_id
 LEFT JOIN submission_periods sp ON sp.term_id=tc.term_id AND sp.year_month=trm.academic_year::text||'-'||substr(l.year_month,6,2)
 LEFT JOIN submission_period_status st ON st.submission_period_id=sp.id AND st.teaching_course_id=l.teaching_course_id AND st.ta_id=l.ta_id
 WHERE l.teaching_course_id=ANY($1) AND l.year_month=$2 AND l.frozen_at<=$3
 AND (COALESCE(st.status,'pending') NOT IN ('exported','finance_sent') OR st.exported_at>$3)
 ) AND NOT EXISTS(SELECT 1 FROM export_batches eb WHERE eb.teaching_course_id=ANY($1) AND eb.months @> ARRAY[$2]::text[] AND eb.generated_at>$3)`, courses, ym, issuedAt).Scan(&current)
	return current, err
}

// The progress of an issued folder follows its frozen membership, even if a
// TA/lecturer is reassigned later. A deliberate send-back still blocks it.
func (s *DocumentProgressService) applyMonthlyIssueReadiness(ctx context.Context, p *TermProgress) error {
	if p.YearMonth == "" {
		return nil
	}
	var issuedAt time.Time
	var courses []uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT generated_at,course_ids,version FROM monthly_payout_batches WHERE term_id=$1 AND year_month=$2 ORDER BY version DESC LIMIT 1`, p.TermID, p.YearMonth).Scan(&issuedAt, &courses, &p.IssuedVersion)
	if err == pgx.ErrNoRows {
		p.AllExported = false
		return nil
	}
	if err != nil {
		return err
	}
	p.TotalCourses = len(courses)
	p.ExportedCourses = 0
	p.UnexportedCourses = []CourseRef{}
	for _, id := range courses {
		current, e := s.export.MonthlyArchiveCurrent(ctx, p.YearMonth, issuedAt, []uuid.UUID{id})
		if e != nil {
			return e
		}
		if current {
			p.ExportedCourses++
			continue
		}
		var ref CourseRef
		if e = s.pool.QueryRow(ctx, `SELECT code,name_th FROM teaching_courses WHERE id=$1`, id).Scan(&ref.Code, &ref.NameTH); e != nil {
			return e
		}
		p.UnexportedCourses = append(p.UnexportedCourses, ref)
	}
	p.AllExported = p.TotalCourses > 0 && p.ExportedCourses == p.TotalCourses
	return nil
}

// Exemption from time submission is not an exemption from appointment or
// payment-document approval. Historical/frozen documents remain reprintable.
func (s *ExportService) monthlyLumpPrerequisites(ctx context.Context, courseID uuid.UUID, months []string) ([]ExportBlocker, error) {
	if len(months) != 1 {
		return nil, nil
	}
	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id=$1`, courseID).Scan(&termID); err != nil {
		return nil, err
	}
	legacy, err := s.LegacyPayoutMonths(ctx, termID)
	if err != nil {
		return nil, err
	}
	if legacy[months[0]] {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT u.id,COALESCE(NULLIF(tp.prefix,''),NULLIF(u.title,''),'')||COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,''),`+AppointedSQL("tc.id", "u.id")+`
 FROM teaching_courses tc JOIN sections sec ON sec.teaching_course_id=tc.id
 JOIN ta_request_assignments a ON a.section_id=sec.id AND a.state<>'dropped'
 JOIN ta_requests r ON r.id=a.request_id AND r.status='approved'
 JOIN users u ON u.id=a.ta_id LEFT JOIN ta_profiles tp ON tp.user_id=u.id
 WHERE tc.id=$1 AND sec.track='special' AND a.level::text IN ('master','phd')
 AND NOT EXISTS(SELECT 1 FROM grad_lump_ledger l WHERE l.teaching_course_id=tc.id AND l.ta_id=u.id AND l.year_month=$2)`, courseID, months[0])
	if err != nil {
		return nil, err
	}
	type person struct {
		id        uuid.UUID
		name      string
		appointed bool
	}
	people := []person{}
	for rows.Next() {
		var p person
		if err = rows.Scan(&p.id, &p.name, &p.appointed); err != nil {
			rows.Close()
			return nil, err
		}
		people = append(people, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []ExportBlocker{}
	for _, p := range people {
		if !p.appointed {
			out = append(out, ExportBlocker{Kind: "not_appointed", TAName: p.name, Months: []string{thaiSelectedMonthsLabel(months)}})
		}
		issue, e := payoutIssue(ctx, s.pool, p.id)
		if e != nil {
			return nil, e
		}
		if issue != "" {
			out = append(out, ExportBlocker{Kind: "profile", TAName: p.name, Issue: issue, Months: []string{thaiSelectedMonthsLabel(months)}})
		}
	}
	return out, nil
}
