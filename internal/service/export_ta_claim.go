// export_ta_claim.go hands a TA their OWN claim sheet, so they can check the
// hours the office will bill against their own records before (or after) staff
// export the course.
//
// It is the same sheet staff download, cut down to one person — not a second
// rendering. ป.ตรี gets its blocks from บันทึกเวลา (ปกติ/พิเศษ) of the combined
// book; บัณฑิตศึกษา, which has no ใบเบิกเวลา of its own, gets its row of the
// graduate หลักฐาน sheets. Nothing else rides along: no หลักฐาน sheet listing
// classmates, no ปะหน้าจ่ายตรง, no timetable.
//
// Strictly a read. Unlike the staff ZIP it locks nothing, records no batch and
// skips the export gates — a TA checking their hours mid-term is the point, and
// the figures are the live ones (approved rows only), which the file says.
package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

// BuildTAClaimWorkbook renders taID's own claim sheet for one course and the
// file name to send it under. It refuses a TA who is not on the course
// (ErrForbidden) and one with nothing approved yet (Invalid).
func (s *ExportService) BuildTAClaimWorkbook(ctx context.Context, taID, courseID uuid.UUID) ([]byte, string, error) {
	var onCourse bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM ta_request_assignments a
		               JOIN sections sec ON sec.id = a.section_id
		               WHERE sec.teaching_course_id = $1 AND a.ta_id = $2
		                 AND a.state <> 'dropped')`, courseID, taID).Scan(&onCourse); err != nil {
		return nil, "", err
	}
	if !onCourse {
		return nil, "", ErrForbidden
	}

	f := excelize.NewFile()
	defer f.Close()
	st, err := newClaimStyles(f)
	if err != nil {
		return nil, "", err
	}
	wrote := false
	var courseCode string
	var year, sem int

	book, err := s.collectCombinedBook(ctx, courseID, nil)
	if err != nil {
		return nil, "", err
	}
	courseCode, year, sem = book.CourseCode, book.AcademicYear, book.Semester
	for _, side := range []struct {
		sheet, trackTH string
		people         []claimant
	}{
		{sheetClaimRegular, "ภาคปกติ", book.Regular},
		{sheetClaimSpecial, "โครงการพิเศษ", book.Special},
	} {
		mine := onlyClaimant(side.people, taID)
		if len(mine) == 0 {
			continue
		}
		if err := writeClaimSheet(f, st, side.sheet, side.trackTH, book, mine); err != nil {
			return nil, "", err
		}
		wrote = true
	}

	grad, err := s.collectGradEvidence(ctx, courseID, nil)
	if err != nil {
		return nil, "", err
	}
	if grad != nil {
		for _, side := range []struct {
			sheet  string
			people []gradEvidencePerson
			lump   bool
		}{
			{sheetGradEvidenceRegular, grad.Regular, false},
			{sheetGradEvidenceSpecial, grad.Special, true},
		} {
			var mine []gradEvidencePerson
			for _, p := range side.people {
				if p.TAID == taID {
					mine = append(mine, p)
				}
			}
			if len(mine) == 0 {
				continue
			}
			if err := writeGradEvidenceSheet(f, st, side.sheet, grad, mine, side.lump); err != nil {
				return nil, "", err
			}
			wrote = true
		}
	}

	if !wrote {
		return nil, "", Invalid("ยังไม่มีบันทึกเวลาที่อาจารย์อนุมัติแล้วในวิชานี้ จึงยังไม่มีข้อมูลสำหรับใบเบิก")
	}
	f.DeleteSheet("Sheet1")
	f.SetActiveSheet(0)
	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("%d_%d_%s-ใบเบิก.xlsx", year, sem, courseCodesFileSafe(courseCode))
	if year == 0 {
		name = courseCodesFileSafe(courseCode) + "-ใบเบิก.xlsx"
	}
	return buf.Bytes(), name, nil
}

// onlyClaimant keeps taID's block, numbered 1 — the ลำดับ on a one-person
// sheet is theirs, not their place in the course list.
func onlyClaimant(cs []claimant, taID uuid.UUID) []claimant {
	for _, c := range cs {
		if c.TAID == taID {
			return []claimant{c}
		}
	}
	return nil
}
