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

// AdminOfficer is one entry in the FIXED roster of administrative seats that
// feed generated official documents (dean, vice deans, head of department,
// ...). Staff assign or reassign who holds an existing seat via
// /settings/admin-officers; the roster itself (which seats exist) is fixed —
// there is no create or delete from this API, only Upsert against an
// existing ID and List.
//
// FullName and AcademicPrefix are never typed directly — both are resolved
// server-side from the linked UserID at save time (see Upsert and
// userTitleToPrefix), so the roster can't drift into a name or prefix that
// doesn't match the actual account. Title is likewise never client-supplied —
// it is the seat's own fixed identity, read back from the row being updated.
type AdminOfficer struct {
	// ID identifies an EXISTING seat — Upsert requires it; there is no create.
	ID             uuid.UUID `json:"id" validate:"required"`
	UserID         uuid.UUID `json:"user_id" validate:"required"`
	AcademicPrefix string    `json:"academic_prefix"`
	FullName       string    `json:"full_name"`
	Title          string    `json:"title"`
	IsActive       bool      `json:"is_active"`
	// IsDean is derived from Title, never stored. Sent to the client so the
	// appointment screen can warn that a deputy will sign as acting without
	// re-implementing the rule — see signer_authority.go.
	IsDean bool `json:"is_dean"`
	// IsHead marks the head-of-department seat, which certifies claim forms.
	// Derived the same way and for the same reason as IsDean.
	IsHead bool `json:"is_head"`
	// LinkedEmail/LinkedActive describe the account UserID points at, so the
	// settings screen can show which account backs a seat and flag one whose
	// account has since been deactivated. Nil only for a pre-migration row
	// nobody has re-linked yet.
	LinkedEmail  *string `json:"linked_email,omitempty"`
	LinkedActive *bool   `json:"linked_active,omitempty"`
}

type AdminOfficerService struct {
	pool *pgxpool.Pool
	aud  *audit.Auditor
}

// userTitleToPrefix mirrors the frontend's USER_TITLE_TO_PREFIX
// (settings/page.tsx) — accounts use the abbreviated academic-title
// vocabulary (see staff/users TITLE_OPTIONS), documents spell it out in
// full. Kept in sync by hand; both lists are short and rarely change.
var userTitleToPrefix = map[string]string{
	"อาจารย์": "อาจารย์",
	"อ. ดร.":  "ดร.",
	"ผศ.":     "ผู้ช่วยศาสตราจารย์",
	"ผศ. ดร.": "ผู้ช่วยศาสตราจารย์ ดร.",
	"รศ. ดร.": "รองศาสตราจารย์ ดร.",
	"ศ. ดร.":  "ศาสตราจารย์ ดร.",
}

func (s *AdminOfficerService) List(ctx context.Context, includeInactive bool) ([]AdminOfficer, error) {
	q := `SELECT ao.id, ao.user_id, ao.academic_prefix, ao.full_name, ao.title, ao.is_active,
	             u.email, u.is_active
	      FROM admin_officers ao
	      LEFT JOIN users u ON u.id = ao.user_id`
	if !includeInactive {
		q += ` WHERE ao.is_active = TRUE`
	}
	q += ` ORDER BY ao.created_at`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminOfficer{}
	for rows.Next() {
		var o AdminOfficer
		var userID *uuid.UUID
		if err := rows.Scan(&o.ID, &userID, &o.AcademicPrefix, &o.FullName, &o.Title, &o.IsActive,
			&o.LinkedEmail, &o.LinkedActive); err != nil {
			return nil, err
		}
		if userID != nil {
			o.UserID = *userID
		}
		o.IsDean = IsDeanTitle(o.Title)
		o.IsHead = IsHeadTitle(o.Title)
		out = append(out, o)
	}
	return out, nil
}

// Upsert reassigns an EXISTING seat — who holds it (UserID) and whether it's
// currently active. There is no create path: in.ID must name a real row, and
// its Title is read back from that row rather than trusted from the client
// (see the type's doc comment for why).
func (s *AdminOfficerService) Upsert(ctx context.Context, actor uuid.UUID, in AdminOfficer) (*AdminOfficer, error) {
	if in.ID == uuid.Nil {
		return nil, Invalid("ตำแหน่งฝ่ายบริหารเป็นรายการคงที่ ไม่สามารถเพิ่มตำแหน่งใหม่ได้")
	}
	if in.UserID == uuid.Nil {
		return nil, Invalid("กรุณาเลือกบัญชีผู้ใช้ที่ต้องการมอบหมายตำแหน่งนี้")
	}
	if err := s.pool.QueryRow(ctx, `SELECT title FROM admin_officers WHERE id=$1`, in.ID).Scan(&in.Title); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, Invalid("ไม่พบตำแหน่งฝ่ายบริหารนี้")
		}
		return nil, err
	}
	// FullName and AcademicPrefix always track the linked account — never the
	// client's own text — so the roster can't say a different name or prefix
	// than the account does.
	var firstName, lastName string
	var userTitle *string
	if err := s.pool.QueryRow(ctx,
		`SELECT first_name, last_name, title FROM users WHERE id=$1 AND deleted_at IS NULL`,
		in.UserID).Scan(&firstName, &lastName, &userTitle); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, Invalid("ไม่พบบัญชีผู้ใช้ที่เลือก")
		}
		return nil, err
	}
	in.FullName = strings.TrimSpace(firstName + " " + lastName)
	in.AcademicPrefix = ""
	if userTitle != nil {
		in.AcademicPrefix = userTitleToPrefix[strings.TrimSpace(*userTitle)]
	}
	// Derived, so a client that posts it back cannot contradict the title.
	in.IsDean = IsDeanTitle(in.Title)
	in.IsHead = IsHeadTitle(in.Title)

	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "admin_officer.reassign", Entity: "admin_officer", EntityID: in.ID.String(), After: in},
		func(tx pgx.Tx) error {
			res, err := tx.Exec(ctx, `
				UPDATE admin_officers
				SET user_id=$2, academic_prefix=$3, full_name=$4, is_active=$5, updated_at=NOW()
				WHERE id=$1`,
				in.ID, in.UserID, in.AcademicPrefix, in.FullName, in.IsActive)
			if err != nil {
				return err
			}
			if res.RowsAffected() == 0 {
				return ErrNotFound
			}
			return nil
		}); err != nil {
		if isUniqueViolation(err) {
			return nil, Invalid("บัญชีนี้ถือตำแหน่งฝ่ายบริหารที่เปิดใช้งานอยู่แล้ว ปิดใช้งานรายชื่อเดิมก่อน")
		}
		return nil, err
	}
	return &in, nil
}
