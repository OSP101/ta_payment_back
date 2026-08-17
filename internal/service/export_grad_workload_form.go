// export_grad_workload_form.go feeds docxgen's แบบแสดงรายละเอียดภาระงานของผู้ช่วยสอน
// — the month-by-month breakdown that backs a graduate TA's hourly claim.
//
// The evidence sheet (export_grad_evidence.go) says a graduate TA worked 54
// hours and was paid 2,700 บาท. This form is what the lecturer signs to say
// WHAT those hours were: so many spent helping teach, so many preparing, so
// many marking, month by month. The college files one per TA alongside the
// evidence sheet; see docs/14.CP363761-บัณฑิต.docx.
//
// The form covers REGULAR-TRACK hours only, and is produced for whoever has
// any. That is a per-ASSIGNMENT test, not a per-person one: a graduate TA can
// hold both a regular-track and a special-track assignment on one course, and
// their regular hours still need this form even though their special-track pay
// does not. Filtering the PERSON out for holding a grad-special assignment
// silently dropped a real TA's whole form on the live CP423434.
//
// A TA with only special-track work gets no form, which falls out of the same
// rule rather than needing one of its own: เหมาจ่าย pay is a flat term lump
// with no logged hours behind it, so there is nothing to break down and a form
// full of blanks would say nothing the evidence sheet does not.
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"ta-payment-back/internal/docxgen"
)

// GradWorkloadForm is one TA's rendered .docx plus the name the ZIP entry is
// built from.
type GradWorkloadForm struct {
	TAID     uuid.UUID
	FullName string
	Doc      []byte
}

// workloadActivityRow maps a work_logs.activity onto the three lines the form
// prints.
//
// The mapping is forced by the data model, and one edge deserves naming:
// activity='other' covers BOTH เตรียมการสอน (prep_hrs) and อื่นๆ (other_hrs)
// for graduate TAs — loadAssignmentContext authorizes the activity from either
// field and the work log records only the activity, so the two cannot be told
// apart afterwards. They are printed as เตรียมการสอน because that is the line
// the college's form has and the overwhelmingly common case; a TA whose hours
// were genuinely "อื่นๆ" is still credited the same hours at the same rate, so
// no money moves on the label.
func workloadActivityRow(activity string) string {
	switch activity {
	case "lecture", "lab":
		return "help_teach"
	case "review":
		return "review"
	default:
		return "prep"
	}
}

// BuildGradWorkloadForms renders one form per grad-regular TA on the course.
// A course with none returns an empty slice and no error — most courses have
// no graduate TAs at all, and that is not a failure.
func (s *ExportService) BuildGradWorkloadForms(
	ctx context.Context, courseID uuid.UUID, months []string,
) ([]GradWorkloadForm, error) {
	var (
		termID                            uuid.UUID
		code, nameTH, nameEN              string
		credits, lectureHrs, labHrs, self int
		academicYear, semester            int
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT tc.term_id, tc.code, COALESCE(tc.name_th,''), COALESCE(tc.name_en,''),
		       COALESCE(tc.credits,0), COALESCE(tc.lecture_hrs,0),
		       COALESCE(tc.lab_hrs,0), COALESCE(tc.self_hrs,0),
		       t.academic_year, t.semester
		FROM teaching_courses tc JOIN academic_terms t ON t.id = tc.term_id
		WHERE tc.id = $1`, courseID).Scan(&termID, &code, &nameTH, &nameEN,
		&credits, &lectureHrs, &labHrs, &self, &academicYear, &semester); err != nil {
		return nil, err
	}
	certifier, err := s.ResolveCertifier(ctx, termID)
	if err != nil {
		return nil, err
	}
	var lecturerName string
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(u.title,''),'')||COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,'')
		FROM teaching_lecturers tl JOIN users u ON u.id = tl.lecturer_id
		WHERE tl.teaching_course_id = $1
		ORDER BY tl.is_primary DESC LIMIT 1`, courseID).Scan(&lecturerName)

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.ta_id,
		       COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '')||
		       COALESCE(u.first_name,'')||' '||COALESCE(u.last_name,''),
		       COALESCE(a.student_id_snapshot, u.student_id, ''),
		       a.level::text
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		JOIN sections sec  ON sec.id = a.section_id
		JOIN users u       ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		WHERE sec.teaching_course_id = $1 AND a.state <> 'dropped'
		  AND a.level::text IN ('master','phd')
		ORDER BY 2`, courseID)
	if err != nil {
		return nil, err
	}
	type person struct {
		id              uuid.UUID
		name, studentID string
		level           string
	}
	var people []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.id, &p.name, &p.studentID, &p.level); err != nil {
			rows.Close()
			return nil, err
		}
		people = append(people, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	semTH := map[int]string{1: "ภาคต้น", 2: "ภาคปลาย", 3: "ภาคฤดูร้อน"}[semester]
	courseName := nameEN
	if courseName == "" {
		courseName = nameTH
	}
	certName, _, certOK := certifier.ClaimCells()

	var out []GradWorkloadForm
	for _, p := range people {
		logs, err := s.claimLogsAllMonths(ctx, p.id, courseID, months)
		if err != nil {
			return nil, err
		}
		// Hours per month per form line. Regular track only: the special-track
		// hours of a grad TA belong to a different pay track and a different
		// sheet, and mixing them would overstate this claim.
		byMonth := map[string]map[string]float64{}
		for _, l := range logs {
			if l.Track != "regular" {
				continue
			}
			ym := l.Date.Format("2006-01")
			if byMonth[ym] == nil {
				byMonth[ym] = map[string]float64{}
			}
			byMonth[ym][workloadActivityRow(l.Activity)] += float64(l.EndMin-l.StartMin) / 60
		}
		if len(byMonth) == 0 {
			continue // nothing approved in this slice — no form to sign
		}
		yms := make([]string, 0, len(byMonth))
		for ym := range byMonth {
			yms = append(yms, ym)
		}
		sort.Strings(yms)

		d := docxgen.WorkloadDetailData{
			SemesterLabel: semTH,
			AcademicYear:  fmt.Sprintf("%d", academicYear),
			CourseCode:    code,
			CourseName:    courseName,
			CreditText:    creditText(credits, lectureHrs, labHrs, self),
			LecturerName:  lecturerName,
			TAName:        p.name,
			StudentID:     p.studentID,
			LevelLabel:    "ระดับบัณฑิตศึกษา",
		}
		if certOK {
			d.CertifierName = certName
			d.CertifierTitle = "ตำแหน่ง " + certifier.TitleLine
			d.CertifierActingFor = certifier.ActingFor
		}
		for _, ym := range yms {
			h := byMonth[ym]
			total := h["help_teach"] + h["prep"] + h["review"]
			d.Months = append(d.Months, docxgen.WorkloadMonth{
				Label:     "เดือน " + thaiMonthLabels([]string{ym})[0],
				HelpTeach: workloadHoursText(h["help_teach"]),
				Prep:      workloadHoursText(h["prep"]),
				Review:    workloadHoursText(h["review"]),
				Total:     workloadHoursText(total),
			})
		}
		doc, err := docxgen.BuildWorkloadDetailDOCX(d)
		if err != nil {
			return nil, err
		}
		out = append(out, GradWorkloadForm{TAID: p.id, FullName: p.name, Doc: doc})
	}
	return out, nil
}

// workloadHoursText prints an hour figure the way the college's form does:
// whole hours bare ("6", not "6.0"), halves with one decimal, and NOTHING at
// all for zero — a blank cell reads as "no work of this kind", where a printed
// 0 reads as a claim that was zeroed out.
func workloadHoursText(h float64) string {
	if h <= 0 {
		return ""
	}
	s := fmt.Sprintf("%.1f", h)
	return strings.TrimSuffix(s, ".0")
}
