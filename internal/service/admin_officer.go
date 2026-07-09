package service

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// AdminOfficer is one entry in the executive/administrative roster that feeds
// generated official documents. Staff CRUD these via /settings/admin-officers.
type AdminOfficer struct {
	ID             uuid.UUID `json:"id"`
	AcademicPrefix string    `json:"academic_prefix"`
	FullName       string    `json:"full_name"`
	Title          string    `json:"title"`
	SortOrder      int       `json:"sort_order"`
	IsActive       bool      `json:"is_active"`
}

type AdminOfficerService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

func (s *AdminOfficerService) List(ctx context.Context, includeInactive bool) ([]AdminOfficer, error) {
	q := `SELECT id, academic_prefix, full_name, title, sort_order, is_active
	      FROM admin_officers`
	if !includeInactive {
		q += ` WHERE is_active = TRUE`
	}
	q += ` ORDER BY sort_order, created_at`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminOfficer{}
	for rows.Next() {
		var o AdminOfficer
		if err := rows.Scan(&o.ID, &o.AcademicPrefix, &o.FullName, &o.Title, &o.SortOrder, &o.IsActive); err != nil {
			return nil, err
		}
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
	if in.SortOrder < 0 {
		in.SortOrder = 0
	}

	if in.ID == uuid.Nil {
		in.ID = uuid.New()
		_, err := s.pool.Exec(ctx, `
			INSERT INTO admin_officers (id, academic_prefix, full_name, title, sort_order, is_active)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			in.ID, in.AcademicPrefix, in.FullName, in.Title, in.SortOrder, in.IsActive)
		if err != nil {
			return nil, err
		}
		s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "admin_officer.create", Entity: "admin_officer", EntityID: in.ID.String(), After: in})
		return &in, nil
	}

	res, err := s.pool.Exec(ctx, `
		UPDATE admin_officers
		SET academic_prefix=$2, full_name=$3, title=$4, sort_order=$5, is_active=$6, updated_at=NOW()
		WHERE id=$1`,
		in.ID, in.AcademicPrefix, in.FullName, in.Title, in.SortOrder, in.IsActive)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "admin_officer.update", Entity: "admin_officer", EntityID: in.ID.String(), After: in})
	return &in, nil
}

func (s *AdminOfficerService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	res, err := s.pool.Exec(ctx, `DELETE FROM admin_officers WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "admin_officer.delete", Entity: "admin_officer", EntityID: id.String()})
	return nil
}
