// curriculum.go manages the curricula reference table (migration 0073) — the
// sheet identity ("ITII" for the IT-coded programme, etc.) the two payout
// documents print under, editable from settings instead of hardcoded because
// the college already renamed one programme without touching any derivation
// logic.
package service

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

type Curriculum struct {
	Code       string `json:"code"`
	SheetName  string `json:"sheet_name"`
	FullNameTH string `json:"full_name_th"`
	Level      string `json:"level"`
	SortOrder  int    `json:"sort_order"`
}

// ListCurricula returns every curriculum, in print order.
func (s *TeachingService) ListCurricula(ctx context.Context) ([]Curriculum, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT code, sheet_name, full_name_th, level, sort_order FROM curricula ORDER BY sort_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Curriculum{}
	for rows.Next() {
		var c Curriculum
		if err := rows.Scan(&c.Code, &c.SheetName, &c.FullNameTH, &c.Level, &c.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCurriculumInput is what staff can change about a curriculum row.
// Code and level are fixed at creation — code is the join key every other
// table points at, and level decides which of the two payout documents' rules
// apply, neither of which a rename should be able to move.
type UpdateCurriculumInput struct {
	SheetName  string `json:"sheet_name"`
	FullNameTH string `json:"full_name_th"`
	SortOrder  int    `json:"sort_order"`
}

func (s *TeachingService) UpdateCurriculum(ctx context.Context, actor uuid.UUID, code string, in UpdateCurriculumInput) error {
	sheetName := strings.TrimSpace(in.SheetName)
	fullName := strings.TrimSpace(in.FullNameTH)
	if sheetName == "" {
		return Invalid("กรุณาระบุชื่อชีต")
	}
	if fullName == "" {
		return Invalid("กรุณาระบุชื่อเต็มหลักสูตร")
	}

	var before Curriculum
	if err := s.pool.QueryRow(ctx,
		`SELECT code, sheet_name, full_name_th, level, sort_order FROM curricula WHERE code = $1`,
		code).Scan(&before.Code, &before.SheetName, &before.FullNameTH, &before.Level, &before.SortOrder); err != nil {
		return err // pgx.ErrNoRows maps to 404 via the global error handler
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE curricula SET sheet_name = $2, full_name_th = $3, sort_order = $4 WHERE code = $1`,
		code, sheetName, fullName, in.SortOrder); err != nil {
		return err
	}

	return s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "curriculum.update", Entity: "curriculum", EntityID: code,
		Before: before,
		After:  Curriculum{Code: code, SheetName: sheetName, FullNameTH: fullName, Level: before.Level, SortOrder: in.SortOrder},
	})
}

// validSectionCurricula mirrors the CHECK constraint on sections.curriculum
// (migration 0073) — kept here so a bad value is rejected with a Thai message
// instead of a raw constraint-violation one.
var validSectionCurricula = map[string]bool{
	"CS": true, "IT": true, "GIS": true, "AI": true, "CY": true, "OTHER": true, "KKBS": true,
}

// UpdateSectionCurriculum lets staff correct which programme a section's
// student count feeds. This is the override the migration 0068/0073 comments
// promised: import derives sections.curriculum from ReservedFor, but ReservedFor
// cannot itself distinguish "KKBS" (one specific other faculty) from every
// other faculty that also lands in the generic OTHER bucket — only a human
// looking at a specific course can.
func (s *TeachingService) UpdateSectionCurriculum(ctx context.Context, actor, sectionID uuid.UUID, curriculum string) error {
	if !validSectionCurricula[curriculum] {
		return Invalid("หลักสูตรไม่ถูกต้อง")
	}
	ct, err := s.pool.Exec(ctx, `UPDATE sections SET curriculum = $2 WHERE id = $1`, sectionID, curriculum)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "section.curriculum.update", Entity: "section", EntityID: sectionID.String(),
		After: map[string]any{"curriculum": curriculum},
	})
}

// graduateCurriculumFromUndergrad maps an undergrad-shaped curriculum code
// (from sections.curriculum / curriculumFromReserved) to the graduate
// curriculum row a graduate-LEVEL course under the same registrar prefix
// prints under. Migration 0073 seeded CS_GRAD ("CS&IT", combining the CS and
// IT undergrad programmes) and DSAI_GRAD ("DS&AI", the closest existing match
// for AI) ahead of the graduate courses themselves. GIS/CY/OTHER have no
// graduate curriculum row yet — a graduate course tagged one of those stays
// unrouted (the same "ยังไม่ทราบหลักสูตร" warning an unrecognised undergrad
// token gets) until the college adds one, rather than being guessed onto an
// unrelated sheet.
func graduateCurriculumFromUndergrad(code string) string {
	switch code {
	case "CS", "IT":
		return "CS_GRAD"
	case "AI":
		return "DSAI_GRAD"
	default:
		return ""
	}
}

// printCurriculumCode resolves the FINAL curricula.code a course prints
// under on ใบ A / ใบ B, combining the token-derived programme (curriculum)
// with the course's own level. One sections.curriculum value routes to two
// different curricula rows depending on level — migration 0073's own comment,
// which is why this cannot be a foreign key. Only applies to courses NOT in a
// confirmed course_groups row; a confirmed group's CurriculumCode was already
// chosen explicitly by staff and must not be re-derived.
func printCurriculumCode(curriculum, level string) string {
	if level != "graduate" {
		return curriculum
	}
	return graduateCurriculumFromUndergrad(curriculum)
}
