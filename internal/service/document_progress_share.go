package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

/* -------------------------------------------------------------------------- */
/* Public share links for the document-progress board                        */
/* -------------------------------------------------------------------------- */

// ShareLink is a staff-issued public URL onto one term's document-progress
// board — the link an officer pastes into the department LINE group so
// lecturers and TAs can check where the paperwork is without an account.
type ShareLink struct {
	ID            uuid.UUID `json:"id"`
	TermID        uuid.UUID `json:"term_id"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedByName string    `json:"created_by_name,omitempty"`
}

// GetShareLink returns the term's current live link, or ErrNotFound if staff
// has never issued one (or revoked the last one).
func (s *DocumentProgressService) GetShareLink(ctx context.Context, termID uuid.UUID) (*ShareLink, error) {
	var l ShareLink
	err := s.pool.QueryRow(ctx, `
		SELECT l.id, l.term_id, l.created_at, COALESCE(u.first_name || ' ' || u.last_name, '')
		FROM document_progress_share_links l
		LEFT JOIN users u ON u.id = l.created_by
		WHERE l.term_id = $1 AND l.revoked_at IS NULL`, termID).Scan(
		&l.ID, &l.TermID, &l.CreatedAt, &l.CreatedByName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &l, nil
}

// CreateShareLink issues a new live link for the term. Idempotent: if one is
// already live, that same row comes back rather than a second, redundant
// link — an officer clicking "สร้างลิงก์" twice must not end up wondering
// which of two posted links is the real one.
func (s *DocumentProgressService) CreateShareLink(ctx context.Context, actor, termID uuid.UUID) (*ShareLink, error) {
	if existing, err := s.GetShareLink(ctx, termID); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var id uuid.UUID
	var createdAt time.Time
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO document_progress_share_links (term_id, created_by)
		VALUES ($1, $2)
		RETURNING id, created_at`, termID, actor).Scan(&id, &createdAt); err != nil {
		return nil, err
	}
	if s.aud != nil {
		_ = s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "document_progress_share_link.create",
			Entity: "academic_term", EntityID: termID.String(), After: map[string]any{"link_id": id.String()}})
	}
	return &ShareLink{ID: id, TermID: termID, CreatedAt: createdAt}, nil
}

// RevokeShareLink turns off the term's live link. The row stays (so it
// remains in the audit trail) but PublicResolveTerm reports it gone — the
// same "not found" an unknown id gets, so a stale link cannot be told apart
// from one that never existed.
func (s *DocumentProgressService) RevokeShareLink(ctx context.Context, actor, termID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE document_progress_share_links
		SET revoked_at = NOW(), revoked_by = $2
		WHERE term_id = $1 AND revoked_at IS NULL`, termID, actor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if s.aud != nil {
		_ = s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "document_progress_share_link.revoke",
			Entity: "academic_term", EntityID: termID.String()})
	}
	return nil
}

// PublicResolveTerm turns a share-link id into the term it points at, plus a
// "2569/1" label for the page's own header (a public reader has no /terms
// list to read a name from). Anything not currently live — unknown id or a
// revoked one — comes back as ErrNotFound, identically, so the endpoint
// cannot be used to probe which links used to exist.
func (s *DocumentProgressService) PublicResolveTerm(ctx context.Context, linkID uuid.UUID) (termID uuid.UUID, termLabel string, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT l.term_id, t.academic_year::text || '/' || t.semester::text
		FROM document_progress_share_links l
		JOIN academic_terms t ON t.id = l.term_id
		WHERE l.id = $1 AND l.revoked_at IS NULL`, linkID).Scan(&termID, &termLabel)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, "", ErrNotFound
		}
		return uuid.Nil, "", err
	}
	return termID, termLabel, nil
}
