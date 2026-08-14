// appointment_order.go orchestrates the "ใบแต่งตั้งทีเอ (คำสั่ง)" export:
// staff pick a term + fill in metadata (order number, dates, signer), the
// service queries every approved TA roster for that term, and renders the
// คำสั่ง as a Word (.docx) file. PDF was dropped 06/08/2026 — see renderBundle.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/docxgen"
)

// AppointmentOrderService produces the คำสั่งแต่งตั้ง .docx for a term.
type AppointmentOrderService struct {
	pool    *pgxpool.Pool
	aud     *audit.Auditor
	fontDir string
}

// AppointmentOrderInput is the request payload from staff.
type AppointmentOrderInput struct {
	TermID          uuid.UUID `json:"term_id" validate:"required"`
	OrderNo         string    `json:"order_no" validate:"required,max=200"`
	OrderDate       string    `json:"order_date" validate:"required,max=200"` // "24 มกราคม 2569"
	EffectiveDate   string    `json:"effective_date" validate:"required,max=200"`
	SignerOfficerID uuid.UUID `json:"signer_officer_id" validate:"required"`
}

// Build renders the next round as appointment-order-<no>.docx.
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

	// Load signer, and work out whether they hold the dean's seat or are acting
	// in it — the printed signature block differs (see signer_authority.go).
	signer, err := loadSignerAuthority(ctx, s.pool, in.SignerOfficerID)
	if err != nil {
		return nil, "", err
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
		SELECT CASE WHEN tc.level = 'undergrad' THEN 0 ELSE 1 END AS level_bucket,
		       tc.code,
		       COALESCE(NULLIF(tc.name_en, ''), tc.name_th) AS course_name,
		       tc.credits, tc.lecture_hrs, tc.lab_hrs, tc.self_hrs,
		       COALESCE(u.student_id, '') AS student_id,
		       COALESCE(NULLIF(tp.prefix, ''), NULLIF(u.title, ''), '') AS prefix,
		       u.first_name, u.last_name,
		       -- Carried for the round ledger, not for the printed page.
		       tc.id AS tc_id, u.id AS ta_id
		FROM ta_request_assignments a
		JOIN ta_requests r       ON r.id = a.request_id AND r.status = 'approved'
		JOIN users u             ON u.id = a.ta_id
		LEFT JOIN ta_profiles tp ON tp.user_id = u.id
		JOIN sections sec        ON sec.id = a.section_id
		JOIN teaching_courses tc ON tc.id = sec.teaching_course_id
		WHERE tc.term_id = $1
		  -- A section every one of whose sessions clashes with the TA's own
		  -- timetable is not an appointment to print.
		  AND a.state <> 'dropped'
		  -- Rounds: never reprint a name already on an issued order for this
		  -- term. A late round must carry only the stragglers.
		  AND NOT EXISTS (
		      SELECT 1
		      FROM appointment_order_items it
		      JOIN appointment_orders o ON o.id = it.appointment_order_id
		      WHERE o.term_id = $1
		        AND it.teaching_course_id = tc.id
		        AND it.ta_id = a.ta_id)
		GROUP BY level_bucket, tc.id, tc.code, course_name,
		         tc.credits, tc.lecture_hrs, tc.lab_hrs, tc.self_hrs,
		         u.id, u.student_id, tp.prefix, u.title, u.first_name, u.last_name
		ORDER BY level_bucket,
		         CASE WHEN tc.code LIKE 'SC%' THEN 0
		              WHEN tc.code LIKE 'CP%' THEN 1
		              ELSE 2 END,
		         tc.code, u.first_name, u.last_name`, in.TermID)
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
	// pairs mirrors `list` but keeps the ids, so the ledger records exactly the
	// people who appear on the printed page — no second query that could drift.
	var pairs []AppointmentCandidate
	for rows.Next() {
		var r rosterRow
		var credits, lec, lab, self int
		var prefix string
		var tcID, taID uuid.UUID
		if err := rows.Scan(&r.levelBucket, &r.code, &r.courseName,
			&credits, &lec, &lab, &self, &r.studentID, &prefix, &r.firstName, &r.lastName,
			&tcID, &taID); err != nil {
			return nil, "", err
		}
		pairs = append(pairs, AppointmentCandidate{
			TeachingCourseID: tcID, CourseCode: r.code,
			TAID: taID, TAName: r.firstName + " " + r.lastName,
		})
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
		// Distinguish "nothing yet" from "everyone already appointed" — the
		// second is a success state staff reach at the end of a term, and
		// reporting it as the first sends them hunting for a problem.
		var issued int
		_ = s.pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM appointment_order_items it
			JOIN appointment_orders o ON o.id = it.appointment_order_id
			WHERE o.term_id = $1`, in.TermID).Scan(&issued)
		if issued > 0 {
			return nil, "", Invalid("ออกคำสั่งครบทุกคนแล้วสำหรับภาคเรียนนี้ ไม่มีรายชื่อค้างให้ออกรอบใหม่")
		}
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

	// Dates arrive from the form as ISO (YYYY-MM-DD) and are formatted here into
	// the Thai government style: order date with the era marker
	// ("14 มกราคม พ.ศ. 2569"), effective date without ("24 พฤศจิกายน 2568"),
	// matching the registrar template.
	doc := docxgen.AppointmentOrderData{
		OrderNo:         in.OrderNo,
		AcademicYear:    academicYear,
		SemesterLabel:   semLabel,
		OrderDate:       thaiGovDate(in.OrderDate, true),
		EffectiveDate:   thaiGovDate(in.EffectiveDate, false),
		SignerName:      signer.Name,
		SignerTitle:     signer.Title,
		SignerActingFor: signer.ActingFor,
		Levels:          docxLevels,
	}

	// Record the round BEFORE returning the bytes. If the ledger write fails
	// we must not hand over a document, because an unrecorded order gets
	// reprinted in the next round and the same TA is appointed twice on paper.
	round, err := s.nextRoundNo(ctx, in.TermID)
	if err != nil {
		return nil, "", err
	}
	if err := s.recordRound(ctx, actor, in, round, pairs, doc); err != nil {
		return nil, "", err
	}

	docxBytes, name, err := s.renderBundle(doc, round)
	if err != nil {
		return nil, "", err
	}

	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "appointment_order.build",
		Entity: "academic_term", EntityID: in.TermID.String(),
		After: map[string]any{"order_no": in.OrderNo, "count": len(list)},
	}); err != nil {
		return nil, "", err
	}
	return docxBytes, name, nil
}

// renderBundle turns one composed order into the .docx file staff download.
//
// It used to package a PDF alongside the DOCX in a zip; dropped 06/08/2026 at
// the officers' request — the คำสั่ง goes onward as a Word file they finish by
// hand, and the PDF was an extra step of unzipping for a copy nobody filed.
//
// Split out of Build so a reprint can reach it with a document loaded from the
// ledger instead of one just assembled from live tables — that is the whole
// mechanism by which a re-issue is a COPY rather than a fresh document. It
// touches no database: everything it needs is in `d`.
func (s *AppointmentOrderService) renderBundle(d docxgen.AppointmentOrderData, round int) ([]byte, string, error) {
	docxBytes, err := docxgen.BuildAppointmentOrderDOCX(d)
	if err != nil {
		return nil, "", err
	}
	// Sanitize OrderNo so "6/2569" cannot smuggle a path into the filename.
	safeOrderNo := strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(d.OrderNo)
	base := fmt.Sprintf("appointment-order-%s%s", safeOrderNo, appointmentRoundSuffix(round))
	return docxBytes, base + ".docx", nil
}

// Reprint hands back a copy of an order that was already issued.
//
// It renders the snapshot frozen when the order was produced and reads nothing
// else, so a TA who has since changed their name, a corrected credit count, or
// a new dean cannot alter a document that has already been signed. An order
// issued before snapshots existed has nothing to copy and is refused rather
// than rebuilt from today's tables — a "copy" that quietly differs from the
// paper in the file is worse than no copy at all.
func (s *AppointmentOrderService) Reprint(ctx context.Context, actor, orderID uuid.UUID) ([]byte, string, error) {
	var raw []byte
	var round int
	var orderNo string
	if err := s.pool.QueryRow(ctx, `
		SELECT document, round_no, order_no FROM appointment_orders WHERE id = $1`,
		orderID).Scan(&raw, &round, &orderNo); err != nil {
		return nil, "", ErrNotFound
	}
	if len(raw) == 0 {
		return nil, "", Invalid("คำสั่งรอบนี้ออกก่อนระบบจะเก็บสำเนาเอกสาร " +
			"ออกซ้ำให้ไม่ได้ เพราะไม่มีหลักฐานว่าฉบับเดิมพิมพ์ข้อความใดไว้")
	}
	var d docxgen.AppointmentOrderData
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, "", fmt.Errorf("appointment order %s: snapshot unreadable: %w", orderID, err)
	}

	docxBytes, name, err := s.renderBundle(d, round)
	if err != nil {
		return nil, "", err
	}
	// Reprints are audited separately from the original: "who else has a copy of
	// this order, and when did they take it" is a different question from "who
	// issued it", and the round ledger only answers the second.
	if err := s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "appointment_order.reprint",
		Entity: "appointment_order", EntityID: orderID.String(),
		After: map[string]any{"order_no": orderNo, "round_no": round},
	}); err != nil {
		return nil, "", err
	}
	return docxBytes, name, nil
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
