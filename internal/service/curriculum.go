// curriculum.go manages the curricula reference table (migration 0073) — the
// sheet identity ("ITII" for the IT-coded programme, etc.) the two payout
// documents print under, editable from settings instead of hardcoded because
// the college already renamed one programme without touching any derivation
// logic.
package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

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
		`SELECT code, sheet_name, full_name_th, level, sort_order FROM curricula ORDER BY sort_order, code`)
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

// Codes are stable identifiers; names, level and print order are editable.
type UpdateCurriculumInput struct {
	SheetName  string `json:"sheet_name" validate:"required,max=31"`
	FullNameTH string `json:"full_name_th" validate:"required,max=200"`
	Level      string `json:"level" validate:"omitempty,oneof=undergrad graduate"`
	SortOrder  int    `json:"sort_order" validate:"gte=0"`
}

type CreateCurriculumInput struct {
	Code string `json:"code" validate:"required,max=40"`
	UpdateCurriculumInput
}

var curriculumCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,39}$`)

func validateCurriculumDetails(in *UpdateCurriculumInput) error {
	in.SheetName = strings.TrimSpace(in.SheetName)
	in.FullNameTH = strings.TrimSpace(in.FullNameTH)
	if in.SheetName == "" || utf8.RuneCountInString(in.SheetName) > 31 || strings.ContainsAny(in.SheetName, `:\/?*[]`) || strings.HasPrefix(in.SheetName, "'") || strings.HasSuffix(in.SheetName, "'") {
		return Invalid("ชื่อชีตต้องมี 1–31 ตัวอักษร ไม่ใช้ : \\ / ? * [ ] และไม่ขึ้นต้นหรือลงท้ายด้วยอัญประกาศเดี่ยว")
	}
	if in.FullNameTH == "" || utf8.RuneCountInString(in.FullNameTH) > 200 {
		return Invalid("กรุณาระบุชื่อเต็มหลักสูตรไม่เกิน 200 ตัวอักษร")
	}
	if in.SortOrder < 0 {
		return Invalid("ลำดับการพิมพ์ต้องไม่ติดลบ")
	}
	if in.Level != "undergrad" && in.Level != "graduate" {
		return Invalid("กรุณาเลือกระดับการศึกษา")
	}
	return nil
}

func curriculumWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return Conflict("รหัสหลักสูตรหรือชื่อชีตซ้ำกับข้อมูลที่มีอยู่")
		case "23503":
			return Conflict("ไม่สามารถลบหลักสูตรที่มีรายวิชาหรือกลุ่มรายวิชาใช้งานอยู่")
		}
	}
	return err
}

func (s *TeachingService) CreateCurriculum(ctx context.Context, actor uuid.UUID, in CreateCurriculumInput) (*Curriculum, error) {
	in.Code = strings.ToUpper(strings.TrimSpace(in.Code))
	if !curriculumCodePattern.MatchString(in.Code) {
		return nil, Invalid("รหัสหลักสูตรต้องขึ้นต้นด้วย A–Z และใช้ A–Z, 0–9 หรือ _ ไม่เกิน 40 ตัวอักษร")
	}
	if err := validateCurriculumDetails(&in.UpdateCurriculumInput); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	after := Curriculum{in.Code, in.SheetName, in.FullNameTH, in.Level, in.SortOrder}
	if _, err := tx.Exec(ctx, `INSERT INTO curricula(code,sheet_name,full_name_th,level,sort_order) VALUES($1,$2,$3,$4,$5)`, after.Code, after.SheetName, after.FullNameTH, after.Level, after.SortOrder); err != nil {
		return nil, curriculumWriteError(err)
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "curriculum.create", Entity: "curriculum", EntityID: in.Code, After: after}); err != nil {
		return nil, err
	}
	return &after, tx.Commit(ctx)
}

func (s *TeachingService) UpdateCurriculum(ctx context.Context, actor uuid.UUID, code string, in UpdateCurriculumInput) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var before Curriculum
	if err := tx.QueryRow(ctx, `SELECT code,sheet_name,full_name_th,level,sort_order FROM curricula WHERE code=$1 FOR UPDATE`, code).Scan(&before.Code, &before.SheetName, &before.FullNameTH, &before.Level, &before.SortOrder); err != nil {
		return err
	}
	if in.Level == "" {
		in.Level = before.Level
	}
	if err := validateCurriculumDetails(&in); err != nil {
		return err
	}
	if in.Level != before.Level {
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sections WHERE curriculum=$1) OR EXISTS(SELECT 1 FROM course_groups WHERE curriculum_code=$1) OR EXISTS(SELECT 1 FROM sections s JOIN teaching_courses tc ON tc.id=s.teaching_course_id WHERE tc.level='graduate' AND (($1='CS_GRAD' AND s.curriculum IN ('CS','IT')) OR ($1='DSAI_GRAD' AND s.curriculum='AI')))`, code).Scan(&used); err != nil {
			return err
		}
		if used {
			return Conflict("ไม่สามารถเปลี่ยนระดับหลักสูตรที่มีรายวิชาใช้งานอยู่")
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE curricula SET sheet_name=$2,full_name_th=$3,level=$4,sort_order=$5 WHERE code=$1`, code, in.SheetName, in.FullNameTH, in.Level, in.SortOrder); err != nil {
		return curriculumWriteError(err)
	}
	after := Curriculum{code, in.SheetName, in.FullNameTH, in.Level, in.SortOrder}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "curriculum.update", Entity: "curriculum", EntityID: code, Before: before, After: after}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *TeachingService) DeleteCurriculum(ctx context.Context, actor uuid.UUID, code string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var before Curriculum
	if err := tx.QueryRow(ctx, `SELECT code,sheet_name,full_name_th,level,sort_order FROM curricula WHERE code=$1 FOR UPDATE`, code).Scan(&before.Code, &before.SheetName, &before.FullNameTH, &before.Level, &before.SortOrder); err != nil {
		return err
	}
	// Graduate aliases are also used indirectly by legacy imported sections.
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sections s JOIN teaching_courses tc ON tc.id=s.teaching_course_id WHERE tc.level='graduate' AND (($1='CS_GRAD' AND s.curriculum IN ('CS','IT')) OR ($1='DSAI_GRAD' AND s.curriculum='AI')))`, code).Scan(&used); err != nil {
		return err
	}
	if used {
		return Conflict("ไม่สามารถลบหลักสูตรที่มีรายวิชาใช้งานอยู่")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM curricula WHERE code=$1`, code); err != nil {
		return curriculumWriteError(err)
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "curriculum.delete", Entity: "curriculum", EntityID: code, Before: before}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *TeachingService) validateCurriculum(ctx context.Context, code string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM curricula WHERE code=$1)`, code).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return Invalid("หลักสูตรไม่ถูกต้อง")
	}
	return nil
}

// Legacy defaults are only used by pure REG validation tests. Live review loads the database.
var validSectionCurricula = map[string]bool{"CS": true, "IT": true, "GIS": true, "AI": true, "CY": true, "OTHER": true, "KKBS": true}

// UpdateSectionCurriculum lets staff correct which programme a section's
// student count feeds. This is the override the migration 0068/0073 comments
// promised: import derives sections.curriculum from ReservedFor, but ReservedFor
// cannot itself distinguish "KKBS" (one specific other faculty) from every
// other faculty that also lands in the generic OTHER bucket — only a human
// looking at a specific course can.
func (s *TeachingService) UpdateSectionCurriculum(ctx context.Context, actor, sectionID uuid.UUID, curriculum string) error {
	if err := s.validateCurriculum(ctx, curriculum); err != nil {
		return err
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
	if mapped := graduateCurriculumFromUndergrad(curriculum); mapped != "" {
		return mapped
	}
	switch curriculum {
	case "GIS", "CY", "OTHER", "KKBS":
		return ""
	default:
		return curriculum
	}
}
