package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BulkCountRow is one pasted line: a registrar code and its real enrolment.
// Regular/Special nil = that column was blank, leave the track as it is.
type BulkCountRow struct {
	Code    string `json:"code"`
	Regular *int   `json:"regular"`
	Special *int   `json:"special"`
	// Sections, when the numbers came from the registrar, is the per-section
	// enrolment under this code. Optional: an Excel paste carries only the
	// track totals, which syncSectionCounts then spreads over the sections.
	Sections []SectionCount `json:"sections,omitempty"`
}

// BulkCountResult is one COURSE (not one line): lines whose codes belong to
// the same course — a merged course opened under CP… and SC… — are summed,
// because the registrar reports each code's enrolment separately and the
// course's count is all of them together.
type BulkCountResult struct {
	CourseID   *uuid.UUID `json:"course_id,omitempty"`
	Codes      []string   `json:"codes"`
	NameTH     string     `json:"name_th,omitempty"`
	OldRegular *int       `json:"old_regular"` // nil = "-" (never entered)
	OldSpecial *int       `json:"old_special"`
	NewRegular *int       `json:"new_regular"` // nil = left unchanged
	NewSpecial *int       `json:"new_special"`
	// Status: "update" (will/did change), "unchanged", "not_found",
	// "locked" (exported or a month already sent out — refused), "confirm"
	// (approved TAs/hours stand on the old budget; applied only with confirm),
	// "zero" (would drop a course that has students to 0 — the registrar not
	// open yet, or a mistyped row; applied only with allowZero), "error".
	// Message carries the Thai reason for the last five.
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	// Warn is advisory and never blocks: e.g. a special count on a course that
	// runs no special section.
	Warn string `json:"warn,omitempty"`
}

func normalizeCode(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

// BulkSetNumStudents updates the real enrolment of many courses in one go —
// the registrar's first file only carries preliminary seat counts, and staff
// get the real numbers weeks later (requested 02/10/2026). Each course goes
// through SetNumStudents exactly as the per-course dialog does, so every gate
// holds: staff only, frozen after export, budget-confirm when approved TAs or
// hours stand on the old figure. dryRun previews the same checks without
// writing. Courses are applied one by one and reported one by one: a locked
// course must not stop the other forty from updating.
//
// allowZero lets a row take a course that has students down to 0. Without it
// such a row is held back as "zero": fetched from the registrar before
// registration opens, every course reads 0 (2569/2 in October 2026), and
// saving that would wipe every course's budget.
func (s *TeachingService) BulkSetNumStudents(ctx context.Context, actor, termID uuid.UUID, rows []BulkCountRow, confirm, allowZero, dryRun bool) ([]BulkCountResult, error) {
	if len(rows) == 0 {
		return nil, Invalid("ไม่มีข้อมูลให้นำเข้า")
	}
	if len(rows) > 2000 {
		return nil, Invalid("ข้อมูลมากเกินไป (เกิน 2,000 แถว) กรุณาแบ่งวางทีละส่วน")
	}
	if priv, err := isPrivileged(ctx, s.pool, actor); err != nil {
		return nil, err
	} else if !priv {
		return nil, Forbidden("จำนวนนักศึกษาต้องให้เจ้าหน้าที่กรอก ข้อมูลมาจากไฟล์ทะเบียน")
	}

	type agg struct {
		res           BulkCountResult
		reg, spc      int
		regSet, spSet bool
		hasSpecial    bool
		lockedExport  bool
		sections      []SectionCount
		secsMatch     bool
	}
	byCourse := map[uuid.UUID]*agg{}
	order := []uuid.UUID{}
	out := []BulkCountResult{}
	seenCode := map[string]bool{}
	for _, r := range rows {
		code := normalizeCode(r.Code)
		if code == "" {
			continue
		}
		if seenCode[code] {
			out = append(out, BulkCountResult{Codes: []string{code}, Status: "error",
				Message: "รหัสวิชานี้ซ้ำกับแถวก่อนหน้า ใช้เฉพาะแถวแรก"})
			continue
		}
		seenCode[code] = true
		if (r.Regular != nil && *r.Regular < 0) || (r.Special != nil && *r.Special < 0) {
			out = append(out, BulkCountResult{Codes: []string{code}, Status: "error", Message: "จำนวนนักศึกษาต้องไม่ติดลบ"})
			continue
		}
		var id uuid.UUID
		var name string
		var oldReg, oldSpc int
		var regEntered, spcEntered, hasSpecial, exported, secsMatch bool
		err := s.pool.QueryRow(ctx, `
			SELECT tc.id, tc.name_th, tc.num_students_regular, tc.num_students_special,
			       -- Sections already adding up to the course row. Saves before
			       -- 03/10/2026 changed only the course, so an "unchanged" row
			       -- must still go through to bring its sections in line.
			       NOT EXISTS (SELECT 1 FROM sections sx WHERE sx.teaching_course_id = tc.id)
			       OR (COALESCE((SELECT SUM(num_students) FROM sections sx WHERE sx.teaching_course_id = tc.id AND sx.track = 'regular'), 0) = tc.num_students_regular
			       AND COALESCE((SELECT SUM(num_students) FROM sections sx WHERE sx.teaching_course_id = tc.id AND sx.track = 'special'), 0) = tc.num_students_special),
			       (tc.num_students_regular > 0 OR tc.num_students_regular_entered),
			       (tc.num_students_special > 0 OR tc.num_students_special_entered),
			       EXISTS (SELECT 1 FROM sections sx WHERE sx.teaching_course_id = tc.id AND sx.track = 'special'),
			       tc.exported_at IS NOT NULL
			  FROM teaching_courses tc
			 WHERE tc.term_id = $1 AND (UPPER(tc.code) = $2 OR $2 = ANY(SELECT UPPER(x) FROM unnest(tc.alt_codes) x))
			 LIMIT 1`, termID, code,
		).Scan(&id, &name, &oldReg, &oldSpc, &secsMatch, &regEntered, &spcEntered, &hasSpecial, &exported)
		if errors.Is(err, pgx.ErrNoRows) {
			out = append(out, BulkCountResult{Codes: []string{code}, Status: "not_found",
				Message: "ไม่พบรหัสวิชานี้ในภาคเรียนที่เลือก"})
			continue
		}
		if err != nil {
			return nil, err
		}
		a := byCourse[id]
		if a == nil {
			a = &agg{hasSpecial: hasSpecial, lockedExport: exported, secsMatch: secsMatch}
			cid := id
			a.res = BulkCountResult{CourseID: &cid, NameTH: name}
			if regEntered {
				v := oldReg
				a.res.OldRegular = &v
			}
			if spcEntered {
				v := oldSpc
				a.res.OldSpecial = &v
			}
			byCourse[id] = a
			order = append(order, id)
		}
		a.res.Codes = append(a.res.Codes, code)
		for _, sc := range r.Sections {
			sc.Code = code
			a.sections = append(a.sections, sc)
		}
		if r.Regular != nil {
			a.reg += *r.Regular
			a.regSet = true
		}
		if r.Special != nil {
			a.spc += *r.Special
			a.spSet = true
		}
	}

	for _, id := range order {
		a := byCourse[id]
		res := a.res
		sort.Strings(res.Codes)
		reg, spc := -1, -1
		if a.regSet {
			v := a.reg
			res.NewRegular, reg = &v, v
		}
		if a.spSet {
			v := a.spc
			res.NewSpecial, spc = &v, v
			if !a.hasSpecial && v > 0 {
				res.Warn = "วิชานี้ไม่มีกลุ่มเรียนภาคพิเศษ แต่มีจำนวนภาคพิเศษในข้อมูล"
			}
		}
		same := func(old, nw *int) bool { return nw == nil || (old != nil && *old == *nw) }
		if same(res.OldRegular, res.NewRegular) && same(res.OldSpecial, res.NewSpecial) && !a.secsMatch && res.Warn == "" {
			res.Warn = "ยอดรวมเท่าเดิม แต่จำนวนใน section รวมไม่เท่ายอดของวิชา บันทึกเพื่อปรับจำนวนใน section ให้ตรง"
		}
		switch {
		case !a.regSet && !a.spSet:
			res.Status, res.Message = "error", "ไม่มีจำนวนนักศึกษาในแถวนี้"
		case same(res.OldRegular, res.NewRegular) && same(res.OldSpecial, res.NewSpecial) && a.secsMatch:
			res.Status = "unchanged"
		case a.lockedExport:
			res.Status, res.Message = "locked", "วิชานี้ส่งออกเอกสารแล้ว แก้จำนวนนักศึกษาไม่ได้จนกว่าผู้ดูแลระบบจะปลดล็อก"
		case !allowZero && dropsToZero(res):
			res.Status, res.Message = "zero", fmt.Sprintf(
				"จำนวนใหม่เป็น 0 ทั้งวิชา (เดิม %d คน) งบของวิชานี้จะเป็น 0 ถ้าระบบทะเบียนยังไม่เปิดลงทะเบียน ให้รอดึงใหม่ภายหลัง",
				orZero(res.OldRegular)+orZero(res.OldSpecial))
		case dryRun:
			res.Status, res.Message = s.previewCounts(ctx, id, reg, spc)
		default:
			err := s.setNumStudents(ctx, actor, id, -1, reg, spc, confirm, a.sections)
			res.Status, res.Message = classifyCountErr(err)
			if err != nil && res.Status == "error" && !isUserFacing(err) {
				return nil, err
			}
		}
		out = append(out, res)
	}
	return out, nil
}

func orZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// dropsToZero: the course has students now and the row leaves it none.
func dropsToZero(r BulkCountResult) bool {
	oldTotal := orZero(r.OldRegular) + orZero(r.OldSpecial)
	reg, spc := orZero(r.OldRegular), orZero(r.OldSpecial)
	if r.NewRegular != nil {
		reg = *r.NewRegular
	}
	if r.NewSpecial != nil {
		spc = *r.NewSpecial
	}
	return oldTotal > 0 && reg+spc == 0
}

// previewCounts runs the write and the budget guard inside a transaction that
// is always rolled back — the only way to ask guardBudgetChange the exact
// question the real save will ask, without saving.
func (s *TeachingService) previewCounts(ctx context.Context, id uuid.UUID, reg, spc int) (string, string) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "error", "ตรวจสอบไม่สำเร็จ"
	}
	defer tx.Rollback(ctx)
	before, err := courseFormulaBudget(ctx, tx, id)
	if err != nil {
		return "error", "ตรวจสอบไม่สำเร็จ"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE teaching_courses SET
		  num_students_regular = CASE WHEN $2 >= 0 THEN $2 ELSE num_students_regular END,
		  num_students_special = CASE WHEN $3 >= 0 THEN $3 ELSE num_students_special END,
		  num_students = (CASE WHEN $2 >= 0 THEN $2 ELSE num_students_regular END)
		               + (CASE WHEN $3 >= 0 THEN $3 ELSE num_students_special END)
		 WHERE id = $1`, id, reg, spc); err != nil {
		return "error", "ตรวจสอบไม่สำเร็จ"
	}
	for _, n := range []int{reg, spc} {
		if err := validateCourseStudents(n); n >= 0 && err != nil {
			return classifyCountErr(err)
		}
	}
	return classifyCountErr(guardBudgetChange(ctx, tx, id, before, false))
}

func classifyCountErr(err error) (string, string) {
	if err == nil {
		return "update", ""
	}
	if errors.Is(err, ErrCourseLocked) {
		return "locked", "วิชานี้ส่งออกเอกสารแล้ว แก้จำนวนนักศึกษาไม่ได้จนกว่าผู้ดูแลระบบจะปลดล็อก"
	}
	var ue *UserError
	if errors.As(err, &ue) {
		if strings.HasPrefix(ue.Msg, BudgetConfirmPrefix) {
			return "confirm", strings.TrimSpace(strings.TrimPrefix(ue.Msg, BudgetConfirmPrefix))
		}
		if ue.Status == 409 {
			return "locked", ue.Msg
		}
		return "error", ue.Msg
	}
	return "error", "บันทึกไม่สำเร็จ"
}

func isUserFacing(err error) bool {
	var ue *UserError
	return errors.As(err, &ue) || errors.Is(err, ErrCourseLocked)
}
