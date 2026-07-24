package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// DocumentProgressService tracks the off-system physical document journey for a
// whole TERM (one batch): the signature round + routing to finance/treasury.
// The board only becomes actionable once every course in the term is exported.
// See migration 0031 for the stage meanings.
type DocumentProgressService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	notify *NotifyService
}

// CourseRef names an un-exported course still blocking the term.
type CourseRef struct {
	Code   string `json:"code"`
	NameTH string `json:"name_th"`
}

// TermProgress is the single per-term progress record + export readiness.
type TermProgress struct {
	TermID uuid.UUID `json:"term_id"`
	// Export readiness — signing can only start once ALL courses that have TAs
	// are exported. TotalCourses counts courses with >=1 approved TA assignment.
	TotalCourses      int         `json:"total_courses"`
	ExportedCourses   int         `json:"exported_courses"`
	AllExported       bool        `json:"all_exported"`
	UnexportedCourses []CourseRef `json:"unexported_courses"`

	Stage             int     `json:"stage"` // 0..5
	TASignedAt        *string `json:"ta_signed_at,omitempty"`
	LecturerSignedAt  *string `json:"lecturer_signed_at,omitempty"`
	CertifierSignedAt *string `json:"certifier_signed_at,omitempty"`
	SentFinanceAt     *string `json:"sent_finance_at,omitempty"`
	SentTreasuryAt    *string `json:"sent_treasury_at,omitempty"`
	Note              *string `json:"note,omitempty"`
	UpdatedByName     *string `json:"updated_by_name,omitempty"`
	UpdatedAt         *string `json:"updated_at,omitempty"`
}

// courseHasTA is the shared predicate for "this course has documents to sign".
const courseHasTA = `EXISTS (
	SELECT 1 FROM ta_request_assignments a
	JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
	JOIN sections s ON s.id = a.section_id
	WHERE s.teaching_course_id = tc.id)`

// exportReadiness returns (total courses-with-TAs, exported count, unexported list).
func (s *DocumentProgressService) exportReadiness(ctx context.Context, termID uuid.UUID) (total, exported int, unexported []CourseRef, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE has_ta),
		       COUNT(*) FILTER (WHERE has_ta AND tc.exported_at IS NOT NULL)
		FROM teaching_courses tc, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
		WHERE tc.term_id = $1`, termID).Scan(&total, &exported)
	if err != nil {
		return
	}
	rows, err := s.pool.Query(ctx, `
		SELECT tc.code, tc.name_th
		FROM teaching_courses tc, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
		WHERE tc.term_id = $1 AND x.has_ta AND tc.exported_at IS NULL
		ORDER BY tc.code`, termID)
	if err != nil {
		return
	}
	defer rows.Close()
	unexported = []CourseRef{}
	for rows.Next() {
		var c CourseRef
		if err = rows.Scan(&c.Code, &c.NameTH); err != nil {
			return
		}
		unexported = append(unexported, c)
	}
	err = rows.Err()
	return
}

// GetByTerm returns the term's document progress + export readiness. Readable by
// any authenticated user (the shared "where are the documents" board).
func (s *DocumentProgressService) GetByTerm(ctx context.Context, termID uuid.UUID) (*TermProgress, error) {
	total, exported, unexported, err := s.exportReadiness(ctx, termID)
	if err != nil {
		return nil, err
	}
	p := &TermProgress{
		TermID:            termID,
		TotalCourses:      total,
		ExportedCourses:   exported,
		AllExported:       total > 0 && exported == total,
		UnexportedCourses: unexported,
	}
	// LEFT-style load: no row yet → stage 0.
	err = s.pool.QueryRow(ctx, `
		SELECT stage,
		       TO_CHAR(ta_signed_at,        'YYYY-MM-DD"T"HH24:MI:SSTZ'),
		       TO_CHAR(lecturer_signed_at,  'YYYY-MM-DD"T"HH24:MI:SSTZ'),
		       TO_CHAR(certifier_signed_at, 'YYYY-MM-DD"T"HH24:MI:SSTZ'),
		       TO_CHAR(sent_finance_at,     'YYYY-MM-DD"T"HH24:MI:SSTZ'),
		       TO_CHAR(sent_treasury_at,    'YYYY-MM-DD"T"HH24:MI:SSTZ'),
		       note, updated_by_name,
		       TO_CHAR(updated_at,          'YYYY-MM-DD"T"HH24:MI:SSTZ')
		FROM document_progress WHERE term_id = $1`, termID).Scan(
		&p.Stage, &p.TASignedAt, &p.LecturerSignedAt, &p.CertifierSignedAt,
		&p.SentFinanceAt, &p.SentTreasuryAt, &p.Note, &p.UpdatedByName, &p.UpdatedAt)
	if err != nil {
		// No row yet is fine — stays stage 0. Any other error propagates.
		if errors.Is(err, pgx.ErrNoRows) {
			return p, nil
		}
		return nil, err
	}
	return p, nil
}

// SetStage moves the term's document progress to `stage` (0..5). Staff/admin
// only, and only once EVERY course with TAs in the term has been exported
// (moving back to 0 is always allowed as a correction).
func (s *DocumentProgressService) SetStage(ctx context.Context, actor, termID uuid.UUID, stage int, note string) error {
	if stage < 0 || stage > 5 {
		return Invalid("ขั้นความคืบหน้าไม่ถูกต้อง")
	}
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if !priv {
		return ErrForbidden
	}
	if stage > 0 {
		total, exported, _, rErr := s.exportReadiness(ctx, termID)
		if rErr != nil {
			return rErr
		}
		if total == 0 {
			return Invalid("ยังไม่มีรายวิชาที่มี TA ในเทอมนี้")
		}
		if exported < total {
			return Invalid("ยังส่งออกเอกสารไม่ครบทุกวิชา — ต้องส่งออกให้ครบก่อนจึงจะเริ่มติดตามการเซ็นได้")
		}
	}
	name := userDisplayName(ctx, s.pool, actor)
	_, err = s.pool.Exec(ctx, `
		INSERT INTO document_progress
		    (term_id, stage, note, updated_by, updated_by_name, updated_at,
		     ta_signed_at, lecturer_signed_at, certifier_signed_at, sent_finance_at, sent_treasury_at)
		VALUES ($1, $2, NULLIF($3,''), $4, $5, now(),
		     CASE WHEN $2>=1 THEN now() END,
		     CASE WHEN $2>=2 THEN now() END,
		     CASE WHEN $2>=3 THEN now() END,
		     CASE WHEN $2>=4 THEN now() END,
		     CASE WHEN $2>=5 THEN now() END)
		ON CONFLICT (term_id) DO UPDATE SET
		     stage           = $2,
		     note            = NULLIF($3,''),
		     updated_by      = $4,
		     updated_by_name = $5,
		     updated_at      = now(),
		     ta_signed_at        = CASE WHEN $2>=1 THEN COALESCE(document_progress.ta_signed_at, now())        ELSE NULL END,
		     lecturer_signed_at  = CASE WHEN $2>=2 THEN COALESCE(document_progress.lecturer_signed_at, now())  ELSE NULL END,
		     certifier_signed_at = CASE WHEN $2>=3 THEN COALESCE(document_progress.certifier_signed_at, now()) ELSE NULL END,
		     sent_finance_at     = CASE WHEN $2>=4 THEN COALESCE(document_progress.sent_finance_at, now())     ELSE NULL END,
		     sent_treasury_at    = CASE WHEN $2>=5 THEN COALESCE(document_progress.sent_treasury_at, now())    ELSE NULL END`,
		termID, stage, note, actor, name)
	if err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "document_progress.set_stage",
		Entity: "academic_term", EntityID: termID.String(),
		After: map[string]int{"stage": stage}})
	return nil
}

// userDisplayName returns "first last" (falling back to email) for a user id —
// a package-level companion to SubmissionPeriodService.userDisplayName so other
// services can snapshot an actor's name.
func userDisplayName(ctx context.Context, pool *pgxpool.Pool, uid uuid.UUID) string {
	var first, last, email string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(first_name,''), COALESCE(last_name,''), COALESCE(email,'')
		 FROM users WHERE id=$1`, uid).Scan(&first, &last, &email); err != nil {
		return ""
	}
	name := first + " " + last
	if name == " " || name == "" {
		return email
	}
	return name
}

/* -------------------------------------------------------------------------- */
/* Per-course signature checklist (migration 0037)                            */
/* -------------------------------------------------------------------------- */

// SignatureItem is one (course × signing-role) checklist row: who is
// responsible and whether they have signed yet.
type SignatureItem struct {
	TeachingCourseID uuid.UUID `json:"teaching_course_id"`
	Code             string    `json:"code"`
	NameTH           string    `json:"name_th"`
	Exported         bool      `json:"exported"`
	Role             string    `json:"role"`
	RoleLabel        string    `json:"role_label"`
	Responsible      string    `json:"responsible"`
	SignedAt         *string   `json:"signed_at,omitempty"`
}

var signatureRoles = []struct{ key, label string }{
	{"ta", "TA เซ็น"},
	{"lecturer", "อาจารย์เซ็น"},
	{"certifier", "ผู้รับรองเซ็น"},
}

// ListChecklist returns three checklist rows (ta/lecturer/certifier) per course
// that has approved TAs in the term, with the responsible names + signed state.
// Readable by any authenticated user.
func (s *DocumentProgressService) ListChecklist(ctx context.Context, termID uuid.UUID) ([]SignatureItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tc.id, tc.code, tc.name_th, (tc.exported_at IS NOT NULL) AS exported,
		  COALESCE((SELECT string_agg(u.first_name || ' ' || u.last_name, ', ')
		            FROM teaching_lecturers tl JOIN users u ON u.id = tl.lecturer_id
		            WHERE tl.teaching_course_id = tc.id), '') AS lecturers,
		  COALESCE((SELECT string_agg(DISTINCT u.first_name || ' ' || u.last_name, ', ')
		            FROM ta_request_assignments a
		            JOIN ta_requests r ON r.id = a.request_id AND r.status = 'approved'
		            JOIN sections s ON s.id = a.section_id
		            JOIN users u ON u.id = a.ta_id
		            WHERE s.teaching_course_id = tc.id), '') AS tas
		FROM teaching_courses tc, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
		WHERE tc.term_id = $1 AND x.has_ta
		ORDER BY tc.code`, termID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type courseRow struct {
		id                          uuid.UUID
		code, name, lecturers, tas  string
		exported                    bool
	}
	var courses []courseRow
	for rows.Next() {
		var c courseRow
		if err := rows.Scan(&c.id, &c.code, &c.name, &c.exported, &c.lecturers, &c.tas); err != nil {
			return nil, err
		}
		courses = append(courses, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Signed timestamps keyed by "tcID|role".
	signed := map[string]*string{}
	srows, err := s.pool.Query(ctx, `
		SELECT teaching_course_id, role, TO_CHAR(signed_at, 'YYYY-MM-DD"T"HH24:MI:SSTZ')
		FROM signature_checklist WHERE term_id = $1 AND signed_at IS NOT NULL`, termID)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var tcID uuid.UUID
		var role string
		var at *string
		if err := srows.Scan(&tcID, &role, &at); err != nil {
			return nil, err
		}
		signed[tcID.String()+"|"+role] = at
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}

	out := []SignatureItem{}
	for _, c := range courses {
		for _, r := range signatureRoles {
			resp := "ผู้รับรอง"
			switch r.key {
			case "ta":
				resp = c.tas
			case "lecturer":
				resp = c.lecturers
			}
			out = append(out, SignatureItem{
				TeachingCourseID: c.id, Code: c.code, NameTH: c.name, Exported: c.exported,
				Role: r.key, RoleLabel: r.label, Responsible: resp,
				SignedAt: signed[c.id.String()+"|"+r.key],
			})
		}
	}
	return out, nil
}

// ToggleSignature marks (or clears) a course-role signature. Staff/admin only.
func (s *DocumentProgressService) ToggleSignature(ctx context.Context, actor, tcID uuid.UUID, role string, signed bool) error {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if !priv {
		return ErrForbidden
	}
	if role != "ta" && role != "lecturer" && role != "certifier" {
		return Invalid("บทบาทไม่ถูกต้อง")
	}
	var termID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id = $1`, tcID).Scan(&termID); err != nil {
		return Invalid("ไม่พบรายวิชา")
	}
	name := userDisplayName(ctx, s.pool, actor)
	signedExpr := "NULL"
	if signed {
		signedExpr = "now()"
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO signature_checklist (term_id, teaching_course_id, role, signed_at, updated_by, updated_by_name, updated_at)
		VALUES ($1, $2, $3, `+signedExpr+`, $4, $5, now())
		ON CONFLICT (teaching_course_id, role) DO UPDATE SET
		  signed_at = `+signedExpr+`, updated_by = $4, updated_by_name = $5, updated_at = now()`,
		termID, tcID, role, actor, name)
	if err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "signature_checklist.toggle",
		Entity: "teaching_course", EntityID: tcID.String(),
		After: map[string]any{"role": role, "signed": signed}})
	return nil
}

// RemindUnsigned emails+notifies every lecturer whose course still lacks the
// lecturer signature, one message per lecturer listing their pending courses.
// Returns how many people were notified. Staff/admin only.
func (s *DocumentProgressService) RemindUnsigned(ctx context.Context, actor, termID uuid.UUID) (int, error) {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return 0, err
	}
	if !priv {
		return 0, ErrForbidden
	}
	if s.notify == nil {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT tl.lecturer_id, tc.code
		FROM teaching_courses tc
		JOIN teaching_lecturers tl ON tl.teaching_course_id = tc.id, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
		LEFT JOIN signature_checklist sc ON sc.teaching_course_id = tc.id AND sc.role = 'lecturer'
		WHERE tc.term_id = $1 AND x.has_ta AND sc.signed_at IS NULL
		ORDER BY tc.code`, termID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	byUser := map[uuid.UUID][]string{}
	order := []uuid.UUID{}
	for rows.Next() {
		var uid uuid.UUID
		var code string
		if err := rows.Scan(&uid, &code); err != nil {
			return 0, err
		}
		if _, ok := byUser[uid]; !ok {
			order = append(order, uid)
		}
		byUser[uid] = append(byUser[uid], code)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, uid := range order {
		body := "มีเอกสารเบิกจ่าย TA ที่รอลายเซ็นของท่านในรายวิชา: " +
			strings.Join(byUser[uid], ", ") +
			"\nกรุณาลงนามเพื่อให้เอกสารเดินทางต่อได้"
		s.notify.Send(ctx, uid, "แจ้งเตือน: เอกสาร TA รอลายเซ็น", body, "/document-progress")
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "signature_checklist.remind",
		Entity: "academic_term", EntityID: termID.String(),
		After: map[string]int{"notified": len(order)}})
	return len(order), nil
}
