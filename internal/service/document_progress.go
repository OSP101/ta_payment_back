package service

import (
	"context"
	"errors"

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
	pool *pgxpool.Pool
	aud  *audit.Auditor
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
		SELECT fc.code, fc.name_th
		FROM teaching_courses tc
		JOIN faculty_courses fc ON fc.id = tc.faculty_course_id, LATERAL (SELECT `+courseHasTA+` AS has_ta) x
		WHERE tc.term_id = $1 AND x.has_ta AND tc.exported_at IS NULL
		ORDER BY fc.code`, termID)
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
