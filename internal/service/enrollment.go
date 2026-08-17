// enrollment.go implements TA education-level history — see migration 0094
// (ta_enrollments) and its own doc comment for why this table exists.
//
// Staff/admin are the only writers of a level TRANSITION (RecordTransition);
// a TA's very own first-ever profile submission is the one exception — see
// DocsService.UpsertProfile, which creates the first ta_enrollments row when
// the user has none yet, and refuses further student_id edits once their
// profile has been approved (the existing ta_profiles.status=='approved'
// gate — see UpsertProfile's own comment for why that boundary was chosen
// over inventing a second one).
package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

type EnrollmentService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

type EnrollmentPeriod struct {
	ID         uuid.UUID  `json:"id"`
	UserID     uuid.UUID  `json:"user_id"`
	StudentID  string     `json:"student_id"`
	StudyLevel string     `json:"study_level"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	CreatedBy  *uuid.UUID `json:"created_by,omitempty"`
	Note       *string    `json:"note,omitempty"`
}

// List returns every enrollment period for a user, newest first — both
// closed and the active one (if any).
func (s *EnrollmentService) List(ctx context.Context, userID uuid.UUID) ([]EnrollmentPeriod, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, student_id, study_level::text, started_at, ended_at, created_by, note
		FROM ta_enrollments WHERE user_id = $1 ORDER BY started_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EnrollmentPeriod{}
	for rows.Next() {
		var e EnrollmentPeriod
		if err := rows.Scan(&e.ID, &e.UserID, &e.StudentID, &e.StudyLevel, &e.StartedAt, &e.EndedAt, &e.CreatedBy, &e.Note); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type RecordTransitionInput struct {
	StudentID  string  `json:"student_id" validate:"required"`
	StudyLevel string  `json:"study_level" validate:"required,oneof=undergrad master phd"`
	Note       *string `json:"note,omitempty" validate:"omitempty,max=500"`
}

// RecordTransition closes the user's current active enrollment (if any) and
// opens a new one, atomically — the user's rule ("advancing to โท/เอก means
// they've finished ตรี") is implemented as one staff action, not two, so it
// is impossible to violate ux_ta_enrollments_one_active by forgetting the
// close step. It then syncs users.student_id/study_level to match, so the
// dozens of existing call sites that read those columns live keep seeing the
// current period without needing to change (see migration 0094's comment).
func (s *EnrollmentService) RecordTransition(ctx context.Context, actor, userID uuid.UUID, in RecordTransitionInput) (*EnrollmentPeriod, error) {
	sid := strings.TrimSpace(in.StudentID)
	if !studentIDPattern(sid) {
		return nil, Invalid("รูปแบบรหัสนักศึกษาไม่ถูกต้อง (XXXXXXXXX-X)")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`, userID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}

	if _, err := tx.Exec(ctx,
		`UPDATE ta_enrollments SET ended_at = NOW() WHERE user_id = $1 AND ended_at IS NULL`,
		userID); err != nil {
		return nil, err
	}

	// ux_ta_enrollments_one_active (migration 0094) backstops the close-then-open
	// above against a concurrent RecordTransition call for the same user: a
	// racing second INSERT fails as a plain 23505 unique-violation, which
	// internal/handler/middleware.go's ErrorHandler already turns into a
	// friendly Thai 409 with no special-casing needed here — same pattern as
	// data_deletion.go's ix_data_deletion_requests_one_pending.
	e := EnrollmentPeriod{ID: uuid.New(), UserID: userID, StudentID: sid, StudyLevel: in.StudyLevel, CreatedBy: &actor, Note: in.Note}
	if err := tx.QueryRow(ctx, `
		INSERT INTO ta_enrollments (id, user_id, student_id, study_level, created_by, note)
		VALUES ($1,$2,$3,$4::study_level,$5,$6)
		RETURNING started_at`,
		e.ID, e.UserID, e.StudentID, e.StudyLevel, e.CreatedBy, e.Note,
	).Scan(&e.StartedAt); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET student_id = $2, study_level = $3::study_level, updated_at = NOW() WHERE id = $1`,
		userID, sid, in.StudyLevel); err != nil {
		return nil, err
	}

	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actor, Action: "ta_enrollment.transition", Entity: "ta_enrollment", EntityID: e.ID.String(),
		After: map[string]any{"user_id": userID, "student_id": sid, "study_level": in.StudyLevel},
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &e, nil
}

// SetSessionScope records which enrollment period sessionID's owner wants
// their session "viewing" — a display filter only (see sessions.
// selected_enrollment_id's doc comment, migration 0096), never something that
// changes what a NEW ta_request_assignment attaches to. Ownership is
// validated here, once, rather than at every read site that later consults
// SelectedEnrollmentID(c): a TA can only ever select one of their OWN
// ta_enrollments rows.
func (s *EnrollmentService) SetSessionScope(ctx context.Context, userID, sessionID, enrollmentID uuid.UUID) error {
	var owned bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ta_enrollments WHERE id = $1 AND user_id = $2)`,
		enrollmentID, userID).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return Forbidden("ช่วงการศึกษานี้ไม่ได้เป็นของบัญชีนี้")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET selected_enrollment_id = $2 WHERE id = $1`,
		sessionID, enrollmentID)
	return err
}
