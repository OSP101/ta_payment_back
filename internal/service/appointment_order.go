// appointment_order.go orchestrates the "ใบแต่งตั้งทีเอ (คำสั่ง)" export:
// staff pick a term + fill in metadata (order number, dates, signer), the
// service queries every approved TA roster for that term, and returns a zip
// containing a PDF and a DOCX rendering. Both formats live behind one
// service so the underlying data query is not duplicated.
package service

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/docxgen"
	"ta-payment-back/internal/pdfgen"
)

// AppointmentOrderService produces the คำสั่งแต่งตั้ง PDF+DOCX bundle for a term.
type AppointmentOrderService struct {
	pool    *pgxpool.Pool
	aud     *audit.Auditor
	fontDir string
}

// AppointmentOrderInput is the request payload from staff.
type AppointmentOrderInput struct {
	TermID          uuid.UUID `json:"term_id"`
	OrderNo         string    `json:"order_no"`
	OrderDate       string    `json:"order_date"`     // "24 มกราคม 2569"
	EffectiveDate   string    `json:"effective_date"`
	SignerOfficerID uuid.UUID `json:"signer_officer_id"`
}

// Build returns a ZIP containing appointment-order.pdf + appointment-order.docx.
func (s *AppointmentOrderService) Build(ctx context.Context, actor uuid.UUID, in AppointmentOrderInput) ([]byte, string, error) {
	if in.OrderNo == "" || in.OrderDate == "" || in.EffectiveDate == "" {
		return nil, "", Invalid("กรุณาระบุคำสั่งที่ / วันที่สั่ง / วันที่ทำการ")
	}

	// Load term metadata.
	var academicYear, semLabel string
	if err := s.pool.QueryRow(ctx, `
		SELECT academic_year::text,
		       CASE semester WHEN 1 THEN 'ภาคต้น' WHEN 2 THEN 'ภาคปลาย' ELSE 'ภาคฤดูร้อน' END
		FROM academic_terms WHERE id = $1`, in.TermID).Scan(&academicYear, &semLabel); err != nil {
		return nil, "", Invalid("ไม่พบภาคเรียนที่ระบุ")
	}

	// Load signer (must be active admin_officer).
	var signerName, signerTitle string
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(academic_prefix,'') || full_name, title
		FROM admin_officers WHERE id = $1`, in.SignerOfficerID).Scan(&signerName, &signerTitle); err != nil {
		return nil, "", Invalid("ไม่พบข้อมูลผู้ลงนามในระบบ")
	}

	// Roster: one line per distinct (TA × course), scoped to approved
	// assignments in this term. A TA teaching several sections of the same
	// course (or both regular + special) is collapsed to a single roster line
	// via GROUP BY. Ordered so undergraduate-level appointments print first,
	// then graduate; within a level, science-faculty courses (SC*, the older
	// programme) print before computing courses (CP*), then any other prefix,
	// then by course code and TA name — matching the registrar's บัญชีแนบท้าย
	// layout.
	rows, err := s.pool.Query(ctx, `
		SELECT CASE WHEN fc.level = 'undergrad' THEN 0 ELSE 1 END AS level_bucket,
		       fc.code,
		       COALESCE(NULLIF(fc.name_en, ''), fc.name_th) AS course_name,
		       fc.credits, fc.lecture_hrs, fc.lab_hrs, fc.self_hrs,
		       COALESCE(u.student_id, '') AS student_id,
		       COALESCE(NULLIF(tp.prefix, ''), NULLIF(u.title, ''), '') AS prefix,
		       u.first_name, u.last_name
		FROM ta_request_assignments a
		JOIN ta_requests r       ON r.id = a.request_id AND r.status = 'approved'
		JOIN users u             ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		JOIN sections sec        ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN faculty_courses fc  ON fc.id = tc.faculty_course_id
		WHERE tc.term_id = $1
		GROUP BY level_bucket, fc.code, course_name,
		         fc.credits, fc.lecture_hrs, fc.lab_hrs, fc.self_hrs,
		         u.student_id, tp.prefix, u.title, u.first_name, u.last_name
		ORDER BY level_bucket,
		         CASE WHEN fc.code LIKE 'SC%' THEN 0
		              WHEN fc.code LIKE 'CP%' THEN 1
		              ELSE 2 END,
		         fc.code, u.first_name, u.last_name`, in.TermID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	type rosterRow struct {
		levelBucket int
		code        string
		courseName  string
		creditText  string
		studentID   string
		firstName   string
		lastName    string
	}
	var list []rosterRow
	for rows.Next() {
		var r rosterRow
		var credits, lec, lab, self int
		var prefix string
		if err := rows.Scan(&r.levelBucket, &r.code, &r.courseName,
			&credits, &lec, &lab, &self, &r.studentID, &prefix, &r.firstName, &r.lastName); err != nil {
			return nil, "", err
		}
		r.creditText = fmt.Sprintf("%d (%d-%d-%d)", credits, lec, lab, self)
		// The template prints the honorific joined to the given name in the
		// name column ("นายชาคริต"). Prefix comes from the TA's own profile
		// (นาย/นาง/นางสาว), falling back to users.title.
		r.firstName = prefix + r.firstName
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(list) == 0 {
		return nil, "", Invalid("ยังไม่มีทีเอที่ได้รับอนุมัติในภาคเรียนนี้")
	}

	// Fold the flat, pre-ordered rows into level → course → appointee groups.
	// The query ordering guarantees rows arrive grouped, so we start a new
	// group only when the level bucket or course code changes.
	levelHeading := func(bucket int) string {
		if bucket == 0 {
			return "รายวิชาระดับปริญญาตรี"
		}
		return "รายวิชาระดับบัณฑิตศึกษา"
	}

	var docxLevels []docxgen.LevelGroup
	curBucket := -1
	curCode := ""
	for _, r := range list {
		if r.levelBucket != curBucket {
			docxLevels = append(docxLevels, docxgen.LevelGroup{Heading: levelHeading(r.levelBucket)})
			curBucket = r.levelBucket
			curCode = ""
		}
		lv := &docxLevels[len(docxLevels)-1]
		if r.code != curCode {
			lv.Courses = append(lv.Courses, docxgen.CourseGroup{
				Code: r.code, Name: r.courseName, CreditText: r.creditText,
			})
			curCode = r.code
		}
		c := &lv.Courses[len(lv.Courses)-1]
		c.Appointees = append(c.Appointees, docxgen.Appointee{
			StudentID: r.studentID, FirstName: r.firstName, LastName: r.lastName,
		})
	}

	// Mirror the same hierarchy for the PDF renderer.
	pdfLevels := make([]pdfgen.AppointmentLevel, 0, len(docxLevels))
	for _, lv := range docxLevels {
		pl := pdfgen.AppointmentLevel{Heading: lv.Heading}
		for _, c := range lv.Courses {
			pc := pdfgen.AppointmentCourse{Code: c.Code, Name: c.Name, CreditText: c.CreditText}
			for _, ap := range c.Appointees {
				pc.Appointees = append(pc.Appointees, pdfgen.AppointmentAppointee{
					StudentID: ap.StudentID, FirstName: ap.FirstName, LastName: ap.LastName,
				})
			}
			pl.Courses = append(pl.Courses, pc)
		}
		pdfLevels = append(pdfLevels, pl)
	}

	// Dates arrive from the form as ISO (YYYY-MM-DD) and are formatted here into
	// the Thai government style: order date with the era marker
	// ("14 มกราคม พ.ศ. 2569"), effective date without ("24 พฤศจิกายน 2568"),
	// matching the registrar template.
	orderDate := thaiGovDate(in.OrderDate, true)
	effectiveDate := thaiGovDate(in.EffectiveDate, false)

	// Render both formats.
	pdfBytes, err := pdfgen.BuildAppointmentOrderPDF(pdfgen.AppointmentOrderInput{
		FontDir: s.fontDir,
		Data: pdfgen.AppointmentOrderData{
			OrderNo:       in.OrderNo,
			AcademicYear:  academicYear,
			SemesterLabel: semLabel,
			OrderDate:     orderDate,
			EffectiveDate: effectiveDate,
			SignerName:    signerName,
			SignerTitle:   signerTitle,
			Levels:        pdfLevels,
		},
	})
	if err != nil {
		// PDF failing (usually missing fontDir) should NOT block the DOCX.
		pdfBytes = nil
	}

	docxBytes, err := docxgen.BuildAppointmentOrderDOCX(docxgen.AppointmentOrderData{
		OrderNo:       in.OrderNo,
		AcademicYear:  academicYear,
		SemesterLabel: semLabel,
		OrderDate:     orderDate,
		EffectiveDate: effectiveDate,
		SignerName:    signerName,
		SignerTitle:   signerTitle,
		Levels:        docxLevels,
	})
	if err != nil {
		return nil, "", err
	}

	// Package as ZIP. Sanitize OrderNo so "6/2569" doesn't become a subfolder
	// inside the archive.
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	safeOrderNo := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(in.OrderNo)
	base := fmt.Sprintf("appointment-order-%s", safeOrderNo)
	if pdfBytes != nil {
		w, _ := zw.Create(base + ".pdf")
		_, _ = w.Write(pdfBytes)
	}
	w, _ := zw.Create(base + ".docx")
	_, _ = w.Write(docxBytes)
	if err := zw.Close(); err != nil {
		return nil, "", err
	}

	s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "appointment_order.build",
		Entity: "academic_term", EntityID: in.TermID.String(),
		After: map[string]any{"order_no": in.OrderNo, "count": len(list)},
	})
	return buf.Bytes(), base + ".zip", nil
}

// ensureBuddhistEra inserts "พ.ศ." before a trailing 4-digit year when staff
// typed a Thai date without the era marker — "14 มกราคม 2569" becomes
// "14 มกราคม พ.ศ. 2569", matching the registrar template. Dates that already
// carry พ.ศ. (or don't end in a year) pass through untouched.
func ensureBuddhistEra(date string) string {
	if strings.Contains(date, "พ.ศ.") {
		return date
	}
	fields := strings.Fields(date)
	if len(fields) < 2 {
		return date
	}
	year := fields[len(fields)-1]
	if len(year) != 4 {
		return date
	}
	for _, r := range year {
		if r < '0' || r > '9' {
			return date
		}
	}
	fields[len(fields)-1] = "พ.ศ. " + year
	return strings.Join(fields, " ")
}

var thaiMonths = [...]string{
	"มกราคม", "กุมภาพันธ์", "มีนาคม", "เมษายน", "พฤษภาคม", "มิถุนายน",
	"กรกฎาคม", "สิงหาคม", "กันยายน", "ตุลาคม", "พฤศจิกายน", "ธันวาคม",
}

// thaiGovDate formats an ISO date ("2026-01-14", Gregorian) into the Thai
// government style: day + full Thai month + Buddhist-era year (+543), e.g.
// "14 มกราคม พ.ศ. 2569" (withEra) or "14 มกราคม 2569" (without). If the input
// isn't ISO it's assumed to be a pre-formatted Thai string and passed through
// (ensureBuddhistEra still fixes a missing พ.ศ. on the order date).
func thaiGovDate(s string, withEra bool) string {
	s = strings.TrimSpace(s)
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		if withEra {
			return ensureBuddhistEra(s)
		}
		return s
	}
	day := t.Day()
	month := thaiMonths[int(t.Month())-1]
	yearBE := t.Year() + 543
	if withEra {
		return fmt.Sprintf("%d %s พ.ศ. %d", day, month, yearBE)
	}
	return fmt.Sprintf("%d %s %d", day, month, yearBE)
}
