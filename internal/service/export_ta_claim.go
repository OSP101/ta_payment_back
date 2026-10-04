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
// skips the export gates — a TA checking their hours mid-term is the point.
//
// It is also not limited to approved rows: a TA may pull it before sending
// anything, to check outside the system. So it counts what is still in play —
// the forecast rule (mergedSittingsForecastCTE): approved, submitted and draft,
// minus rejected rows and drafts forfeited in a closed month. Rows not approved
// yet say so in หมายเหตุ, and every page carries a margin note that this is a
// check copy, not the claim the office files.
package service

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

// TAClaimMonth is one month a TA can pick for their own claim sheet.
type TAClaimMonth struct {
	YearMonth string `json:"year_month"` // Gregorian "2026-06"
	Label     string `json:"label"`      // "มิถุนายน 2569"
	// Hours is everything the sheet would print (the check-copy rule);
	// ApprovedHours is the part of it the lecturer has approved.
	Hours         float64 `json:"hours"`
	ApprovedHours float64 `json:"approved_hours"`
}

// taCheckCopyKey marks a context as building a TA's check copy — the claim
// readers then take rows still in play instead of approved rows only.
type taCheckCopyKey struct{}

func withTACheckCopy(ctx context.Context) context.Context {
	return context.WithValue(ctx, taCheckCopyKey{}, true)
}

func isTACheckCopy(ctx context.Context) bool {
	v, _ := ctx.Value(taCheckCopyKey{}).(bool)
	return v
}

// claimSittingsCTE is the priced-sittings view the claim builders read.
func claimSittingsCTE(ctx context.Context) string {
	if isTACheckCopy(ctx) {
		return mergedSittingsForecastCTE
	}
	return mergedSittingsCTE
}

// claimRowStatusSQL filters work_logs (alias wl, section alias sec) to the rows
// a claim sheet prints — the same rule as claimSittingsCTE.
func claimRowStatusSQL(ctx context.Context) string {
	if isTACheckCopy(ctx) {
		return `wl.status <> 'rejected'
		  AND NOT (wl.status = 'draft' AND ` + forfeitedDraftMonthSQL("wl", "sec") + `)`
	}
	return `wl.status = 'approved'`
}

// settleForClaim is the budget settlement the funded figures come from.
func (s *ExportService) settleForClaim(ctx context.Context, courseID uuid.UUID) (*CourseSettlement, error) {
	if isTACheckCopy(ctx) {
		return s.ForecastCourse(ctx, courseID)
	}
	return s.SettleCourse(ctx, courseID)
}

// assertTAOnCourse refuses (ErrForbidden) a TA with no live assignment on the
// course — the sheet is theirs only if the course is.
func (s *ExportService) assertTAOnCourse(ctx context.Context, taID, courseID uuid.UUID) error {
	var onCourse bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM ta_request_assignments a
		               JOIN sections sec ON sec.id = a.section_id
		               WHERE sec.teaching_course_id = $1 AND a.ta_id = $2
		                 AND a.state <> 'dropped')`, courseID, taID).Scan(&onCourse); err != nil {
		return err
	}
	if !onCourse {
		return ErrForbidden
	}
	return nil
}

// TAClaimMonths lists the course's term months for the download picker, each
// with the caller's hours in it (all that the check copy prints, and the
// approved part), so the TA can see which months would come out empty before
// asking for them. A month outside the term that still holds rows is listed too.
func (s *ExportService) TAClaimMonths(ctx context.Context, taID, courseID uuid.UUID) ([]TAClaimMonth, error) {
	if err := s.assertTAOnCourse(ctx, taID, courseID); err != nil {
		return nil, err
	}
	term, err := s.CourseTermMonths(ctx, courseID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT to_char(wl.work_date, 'YYYY-MM'),
		       SUM(EXTRACT(EPOCH FROM (wl.end_time - wl.start_time)) / 3600),
		       COALESCE(SUM(EXTRACT(EPOCH FROM (wl.end_time - wl.start_time)) / 3600)
		                FILTER (WHERE wl.status = 'approved'), 0)
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		WHERE a.ta_id = $1 AND sec.teaching_course_id = $2
		  AND `+claimRowStatusSQL(withTACheckCopy(ctx))+`
		GROUP BY 1`, taID, courseID)
	if err != nil {
		return nil, err
	}
	hours, approved := map[string]float64{}, map[string]float64{}
	for rows.Next() {
		var ym string
		var h, a float64
		if err := rows.Scan(&ym, &h, &a); err != nil {
			rows.Close()
			return nil, err
		}
		hours[ym], approved[ym] = round2(h), round2(a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []TAClaimMonth{}
	seen := map[string]bool{}
	for _, m := range term {
		seen[m.YearMonth] = true
		out = append(out, TAClaimMonth{YearMonth: m.YearMonth, Label: m.Label,
			Hours: hours[m.YearMonth], ApprovedHours: approved[m.YearMonth]})
	}
	for ym, h := range hours {
		if seen[ym] {
			continue
		}
		var y, mo int
		fmt.Sscanf(ym, "%d-%d", &y, &mo)
		out = append(out, TAClaimMonth{YearMonth: ym, Label: fmt.Sprintf("%s %d", thaiMonthNames[mo], y+543),
			Hours: h, ApprovedHours: approved[ym]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].YearMonth < out[j].YearMonth })
	return out, nil
}

// BuildTAClaimWorkbook renders taID's own claim sheet for one course and the
// file name to send it under, limited to months (Gregorian "YYYY-MM"; empty =
// the whole term). It refuses a TA who is not on the course (ErrForbidden) and
// a selection with no logged time in it (Invalid).
func (s *ExportService) BuildTAClaimWorkbook(ctx context.Context, taID, courseID uuid.UUID, months []string) ([]byte, string, error) {
	if err := s.assertTAOnCourse(ctx, taID, courseID); err != nil {
		return nil, "", err
	}
	for _, m := range months {
		if !yearMonthRe.MatchString(m) {
			return nil, "", Invalid("เดือนที่เลือกไม่ถูกต้อง")
		}
	}
	months = append([]string(nil), months...)
	sort.Strings(months)

	ctx = withTACheckCopy(ctx)
	f := excelize.NewFile()
	defer f.Close()
	st, err := newClaimStyles(f)
	if err != nil {
		return nil, "", err
	}
	wrote := false
	var courseCode string
	var year, sem int

	book, err := s.collectCombinedBook(ctx, courseID, months)
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

	grad, err := s.collectGradEvidence(ctx, courseID, months)
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
				// The เหมาจ่าย row is listed whatever the months; a slice it
				// puts nothing in would print a ฿0 claim.
				if p.TAID == taID && p.total(grad.Months) > 0 {
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
		if len(months) > 0 {
			return nil, "", Invalid("เดือนที่เลือกยังไม่มีบันทึกเวลา จึงยังไม่มีข้อมูลสำหรับใบเบิก")
		}
		return nil, "", Invalid("ยังไม่มีบันทึกเวลาในวิชานี้ จึงยังไม่มีข้อมูลสำหรับใบเบิก")
	}
	f.DeleteSheet("Sheet1")
	// Margin text on every printed page: this copy can hold rows nobody has
	// approved, so it must not pass for the claim the office files.
	hf := &excelize.HeaderFooterOptions{
		OddHeader: "&R&8สำเนาสำหรับ TA ตรวจสอบ ไม่ใช่เอกสารเบิกจ่าย",
		OddFooter: "&C&8พิมพ์จากระบบจ่ายค่าตอบแทน TA &D &T",
	}
	for _, sh := range f.GetSheetList() {
		if err := f.SetHeaderFooter(sh, hf); err != nil {
			return nil, "", err
		}
	}
	f.SetActiveSheet(0)
	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, "", err
	}
	// Same naming as the staff ZIP, month slice included, so two downloads of
	// different months do not overwrite each other in the TA's folder.
	name := fmt.Sprintf("%d_%d_%s", year, sem, courseCodesFileSafe(courseCode))
	if year == 0 {
		name = courseCodesFileSafe(courseCode)
	}
	if len(months) > 0 {
		name += "_" + months[0]
		if len(months) > 1 {
			name += "_" + months[len(months)-1]
		}
	}
	name += "-ใบเบิก.xlsx"
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
