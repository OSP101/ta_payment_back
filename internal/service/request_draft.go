package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RequestDraft is an unsent TA-request form. The payload is opaque to the
// server — the page that writes it is the page that reads it — so the only
// checks here are ownership, size and that it is JSON at all.
type RequestDraft struct {
	Payload   json.RawMessage `json:"payload"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// A form has a handful of TAs with per-section hours; 64 KB is far past that
// and stops the column becoming a dumping ground.
const maxRequestDraftBytes = 64 * 1024

func (s *ExportService) lecturerTeaches(ctx context.Context, actor, courseID uuid.UUID) error {
	var ok bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM teaching_lecturers tl
		                WHERE tl.teaching_course_id = $1 AND tl.lecturer_id = $2)`,
		courseID, actor).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// GetRequestDraft returns the caller's draft for the course, or nil when
// there is none. Drafts are personal: co-lecturers each keep their own.
func (s *ExportService) GetRequestDraft(ctx context.Context, actor, courseID uuid.UUID) (*RequestDraft, error) {
	if err := s.lecturerTeaches(ctx, actor, courseID); err != nil {
		return nil, err
	}
	d := &RequestDraft{}
	err := s.pool.QueryRow(ctx,
		`SELECT payload, updated_at FROM ta_request_drafts WHERE user_id = $1 AND teaching_course_id = $2`,
		actor, courseID).Scan(&d.Payload, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// SaveRequestDraft upserts the caller's draft.
func (s *ExportService) SaveRequestDraft(ctx context.Context, actor, courseID uuid.UUID, payload json.RawMessage) (*RequestDraft, error) {
	if err := s.lecturerTeaches(ctx, actor, courseID); err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload) > maxRequestDraftBytes || !json.Valid(payload) {
		return nil, Invalid("ข้อมูลร่างไม่ถูกต้อง")
	}
	d := &RequestDraft{Payload: payload}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ta_request_drafts (user_id, teaching_course_id, payload, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, teaching_course_id)
		DO UPDATE SET payload = EXCLUDED.payload, updated_at = now()
		RETURNING updated_at`, actor, courseID, payload).Scan(&d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// DeleteRequestDraft drops the caller's draft (after a submit, or on request).
func (s *ExportService) DeleteRequestDraft(ctx context.Context, actor, courseID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM ta_request_drafts WHERE user_id = $1 AND teaching_course_id = $2`, actor, courseID)
	return err
}
