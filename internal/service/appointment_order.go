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

	// Roster: every distinct (TA × course × track × level) with an approved
	// assignment in this term. Ordered by course code so the printed table
	// naturally groups by course.
	rows, err := s.pool.Query(ctx, `
		SELECT a.ta_id, MIN(u.first_name || ' ' || u.last_name),
		       fc.code, sec.track::text, a.level::text
		FROM ta_request_assignments a
		JOIN ta_requests r      ON r.id = a.request_id AND r.status = 'approved'
		JOIN users u            ON u.id = a.ta_id
		JOIN sections sec       ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id
		WHERE tc.term_id = $1
		GROUP BY a.ta_id, fc.code, sec.track, a.level
		ORDER BY fc.code, MIN(u.first_name)`, in.TermID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	type roster struct {
		taID       uuid.UUID
		fullName   string
		courseCode string
		track      string
		level      string
	}
	var list []roster
	for rows.Next() {
		var r roster
		if err := rows.Scan(&r.taID, &r.fullName, &r.courseCode, &r.track, &r.level); err != nil {
			return nil, "", err
		}
		list = append(list, r)
	}
	if len(list) == 0 {
		return nil, "", Invalid("ยังไม่มีทีเอที่ได้รับอนุมัติในภาคเรียนนี้")
	}

	// Old/new lookup — reuse export.isReturningTA logic inline (single-purpose here).
	returningCache := map[uuid.UUID]bool{}
	for _, r := range list {
		if _, ok := returningCache[r.taID]; ok {
			continue
		}
		var yes bool
		_ = s.pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM ta_request_assignments a
				JOIN ta_requests r  ON r.id = a.request_id AND r.status = 'approved'
				JOIN sections sec   ON sec.id = a.section_id
				JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
				JOIN academic_terms t   ON t.id = tc.term_id
				JOIN academic_terms cur ON cur.id = $2
				WHERE a.ta_id = $1
				  AND (t.academic_year < cur.academic_year
				       OR (t.academic_year = cur.academic_year AND t.semester < cur.semester))
			)`, r.taID, in.TermID).Scan(&yes)
		returningCache[r.taID] = yes
	}

	// Build shared appointee slices for both renderers.
	pdfAppointees := make([]pdfgen.AppointmentAppointee, 0, len(list))
	docxAppointees := make([]docxgen.Appointee, 0, len(list))
	for _, r := range list {
		levelTH := r.level
		switch r.level {
		case "undergrad":
			levelTH = "ปริญญาตรี"
		case "master":
			levelTH = "ปริญญาโท"
		case "phd":
			levelTH = "ปริญญาเอก"
		}
		trackTH := r.track
		switch r.track {
		case "regular":
			trackTH = "ภาคปกติ"
		case "special":
			trackTH = "ภาคพิเศษ"
		}
		pdfAppointees = append(pdfAppointees, pdfgen.AppointmentAppointee{
			FullName: r.fullName, Level: levelTH, Track: trackTH,
			CourseCode: r.courseCode, IsReturning: returningCache[r.taID],
		})
		docxAppointees = append(docxAppointees, docxgen.Appointee{
			FullName: r.fullName, Level: levelTH, Track: trackTH,
			CourseCode: r.courseCode, Returning: returningCache[r.taID],
		})
	}

	// Render both formats.
	pdfBytes, err := pdfgen.BuildAppointmentOrderPDF(pdfgen.AppointmentOrderInput{
		FontDir: s.fontDir,
		Data: pdfgen.AppointmentOrderData{
			OrderNo:       in.OrderNo,
			AcademicYear:  academicYear,
			SemesterLabel: semLabel,
			OrderDate:     in.OrderDate,
			EffectiveDate: in.EffectiveDate,
			SignerName:    signerName,
			SignerTitle:   signerTitle,
			Appointees:    pdfAppointees,
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
		OrderDate:     in.OrderDate,
		EffectiveDate: in.EffectiveDate,
		SignerName:    signerName,
		SignerTitle:   signerTitle,
		Appointees:    docxAppointees,
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
