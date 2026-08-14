package service

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// AdminOfficer is one entry in the executive/administrative roster that feeds
// generated official documents. Staff CRUD these via /settings/admin-officers.
type AdminOfficer struct {
	// ID is empty on a create; Upsert treats uuid.Nil as "assign a new one".
	ID             uuid.UUID `json:"id"`
	AcademicPrefix string    `json:"academic_prefix" validate:"omitempty,max=200"`
	FullName       string    `json:"full_name" validate:"required,max=200"`
	Title          string    `json:"title" validate:"required,max=200"`
	IsActive       bool      `json:"is_active"`
	// IsDean is derived from Title, never stored. Sent to the client so the
	// appointment screen can warn that a deputy will sign as acting without
	// re-implementing the rule — see signer_authority.go.
	IsDean bool `json:"is_dean"`
	// IsHead marks the head-of-department seat, which certifies claim forms.
	// Derived the same way and for the same reason as IsDean.
	IsHead bool `json:"is_head"`
}

type AdminOfficerService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

func (s *AdminOfficerService) List(ctx context.Context, includeInactive bool) ([]AdminOfficer, error) {
	q := `SELECT id, academic_prefix, full_name, title, is_active
	      FROM admin_officers`
	if !includeInactive {
		q += ` WHERE is_active = TRUE`
	}
	q += ` ORDER BY created_at`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminOfficer{}
	for rows.Next() {
		var o AdminOfficer
		if err := rows.Scan(&o.ID, &o.AcademicPrefix, &o.FullName, &o.Title, &o.IsActive); err != nil {
			return nil, err
		}
		o.IsDean = IsDeanTitle(o.Title)
		o.IsHead = IsHeadTitle(o.Title)
		out = append(out, o)
	}
	return out, nil
}

func (s *AdminOfficerService) Upsert(ctx context.Context, actor uuid.UUID, in AdminOfficer) (*AdminOfficer, error) {
	in.AcademicPrefix = strings.TrimSpace(in.AcademicPrefix)
	in.FullName = strings.TrimSpace(in.FullName)
	in.Title = strings.TrimSpace(in.Title)
	if in.FullName == "" {
		return nil, Invalid("กรุณาระบุชื่อ-นามสกุล")
	}
	if in.Title == "" {
		return nil, Invalid("กรุณาระบุตำแหน่งบริหาร")
	}
	// Derived, so a client that posts it back cannot contradict the title.
	in.IsDean = IsDeanTitle(in.Title)
	in.IsHead = IsHeadTitle(in.Title)

	if in.ID == uuid.Nil {
		in.ID = uuid.New()
		if err := writeAudited(ctx, s.pool, s.aud,
			audit.Entry{ActorID: &actor, Action: "admin_officer.create", Entity: "admin_officer", EntityID: in.ID.String(), After: in},
			func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					INSERT INTO admin_officers (id, academic_prefix, full_name, title, is_active)
					VALUES ($1,$2,$3,$4,$5)`,
					in.ID, in.AcademicPrefix, in.FullName, in.Title, in.IsActive)
				return err
			}); err != nil {
			return nil, err
		}
		return &in, nil
	}

	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "admin_officer.update", Entity: "admin_officer", EntityID: in.ID.String(), After: in},
		func(tx pgx.Tx) error {
			res, err := tx.Exec(ctx, `
				UPDATE admin_officers
				SET academic_prefix=$2, full_name=$3, title=$4, is_active=$5, updated_at=NOW()
				WHERE id=$1`,
				in.ID, in.AcademicPrefix, in.FullName, in.Title, in.IsActive)
			if err != nil {
				return err
			}
			if res.RowsAffected() == 0 {
				return ErrNotFound
			}
			return nil
		}); err != nil {
		return nil, err
	}
	return &in, nil
}

func (s *AdminOfficerService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "admin_officer.delete", Entity: "admin_officer", EntityID: id.String()},
		func(tx pgx.Tx) error {
			res, err := tx.Exec(ctx, `DELETE FROM admin_officers WHERE id=$1`, id)
			if err != nil {
				return err
			}
			if res.RowsAffected() == 0 {
				return ErrNotFound
			}
			return nil
		})
}
