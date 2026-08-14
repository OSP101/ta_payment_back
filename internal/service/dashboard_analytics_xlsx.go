package service

// The Excel behind the dashboard's "ส่งออกข้อมูล" button — a report the
// management team can open in a meeting, not a raw dump. Four sheets:
//
//	สรุป        cover page: KPI block + two native Excel charts
//	รายเดือน     disbursement by month (+ cumulative), feeds the combo chart
//	รายหลักสูตร  per-curriculum rollup, feeds the bar chart
//	รายวิชา      per-course drill-down, severity-tinted rows
//
// Everything renders from the same TermAnalytics struct the page shows, in the
// same request — a second query path would eventually disagree with the screen
// it claims to export. Figures are static values, not formulas: this is a
// snapshot of settle-priced money, and a formula that recomputed differently
// from the settlement would be a bug wearing a suit.

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/xuri/excelize/v2"
)

// CurriculumTH is the display name per programme group. Exported because the
// frontend chart legend and this workbook must say the same words.
func CurriculumTH(code string) string {
	switch code {
	case "CS":
		return "วิทยาการคอมพิวเตอร์"
	case "IT":
		return "เทคโนโลยีสารสนเทศ"
	case "GIS":
		return "ภูมิสารสนเทศศาสตร์"
	case "AI":
		return "ปัญญาประดิษฐ์"
	case "CY":
		return "ความมั่นคงปลอดภัยไซเบอร์"
	case "OTHER":
		return "คณะอื่น ๆ"
	}
	return "ยังไม่ระบุ"
}

// thaiMonthShortBE turns "2026-06" into "มิ.ย. 69".
func thaiMonthShortBE(ym string) string {
	if len(ym) != 7 {
		return ym
	}
	names := [...]string{"", "ม.ค.", "ก.พ.", "มี.ค.", "เม.ย.", "พ.ค.", "มิ.ย.", "ก.ค.", "ส.ค.", "ก.ย.", "ต.ค.", "พ.ย.", "ธ.ค."}
	y, err1 := strconv.Atoi(ym[:4])
	m, err2 := strconv.Atoi(ym[5:7])
	if err1 != nil || err2 != nil || m < 1 || m > 12 {
		return ym
	}
	return fmt.Sprintf("%s %02d", names[m], (y+543)%100)
}

// The report's palette — the same hues the dashboard uses on screen.
const (
	xlBrand      = "2563C9" // primary blue
	xlBrandSoft  = "DEE9F8"
	xlInk        = "1C2530"
	xlMuted      = "5B6877"
	xlZebra      = "F4F6F9"
	xlGood       = "1A7F4B"
	xlGoodSoft   = "E3F3EA"
	xlWarn       = "B3701A"
	xlWarnSoft   = "FDF1DE"
	xlDanger     = "BB3535"
	xlDangerSoft = "FBE9E9"
	xlLine       = "D5DCE4"
)

// analyticsStyles carries every style ID the sheets share.
type analyticsStyles struct {
	title, subtitle                  int
	kpiLabel, kpiValue, kpiHint      int
	kpiValueGood, kpiValueWarn       int
	header                           int
	text, textZebra                  int
	num, numZebra                    int
	baht, bahtZebra                  int
	pct, pctZebra                    int
	pctWarn, pctDanger               int
	textWarnTint, textDangerTint     int
	numWarnTint, numDangerTint       int
	bahtWarnTint, bahtDangerTint     int
	statusOK, statusWarn, statusOver int
}

func buildAnalyticsStyles(f *excelize.File) (*analyticsStyles, error) {
	s := &analyticsStyles{}
	font := func(size float64, bold bool, color string) *excelize.Font {
		return &excelize.Font{Family: "TH Sarabun New", Size: size, Bold: bold, Color: color}
	}
	borders := func(color string) []excelize.Border {
		return []excelize.Border{
			{Type: "left", Color: color, Style: 1},
			{Type: "right", Color: color, Style: 1},
			{Type: "top", Color: color, Style: 1},
			{Type: "bottom", Color: color, Style: 1},
		}
	}
	fill := func(color string) excelize.Fill {
		return excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{color}}
	}
	mk := func(st *excelize.Style) (int, error) { return f.NewStyle(st) }

	var err error
	center := &excelize.Alignment{Horizontal: "center", Vertical: "center"}
	left := &excelize.Alignment{Horizontal: "left", Vertical: "center", WrapText: true}
	right := &excelize.Alignment{Horizontal: "right", Vertical: "center"}
	const bahtFmt = "#,##0.00"
	const numFmt = "#,##0.0"
	const pctFmt = "0.0%"

	if s.title, err = mk(&excelize.Style{Font: font(26, true, "FFFFFF"), Fill: fill(xlBrand), Alignment: center}); err != nil {
		return nil, err
	}
	if s.subtitle, err = mk(&excelize.Style{Font: font(13, false, "EAF1FC"), Fill: fill(xlBrand), Alignment: center}); err != nil {
		return nil, err
	}
	if s.kpiLabel, err = mk(&excelize.Style{Font: font(14, false, xlMuted), Alignment: left}); err != nil {
		return nil, err
	}
	if s.kpiValue, err = mk(&excelize.Style{Font: font(20, true, xlBrand), Alignment: right, CustomNumFmt: &[]string{bahtFmt}[0]}); err != nil {
		return nil, err
	}
	if s.kpiValueGood, err = mk(&excelize.Style{Font: font(20, true, xlGood), Alignment: right, CustomNumFmt: &[]string{bahtFmt}[0]}); err != nil {
		return nil, err
	}
	if s.kpiValueWarn, err = mk(&excelize.Style{Font: font(20, true, xlWarn), Alignment: right, CustomNumFmt: &[]string{bahtFmt}[0]}); err != nil {
		return nil, err
	}
	if s.kpiHint, err = mk(&excelize.Style{Font: font(12, false, xlMuted), Alignment: left}); err != nil {
		return nil, err
	}
	if s.header, err = mk(&excelize.Style{Font: font(14, true, "FFFFFF"), Fill: fill(xlBrand), Alignment: center, Border: borders(xlBrand)}); err != nil {
		return nil, err
	}

	body := func(numfmt string, align *excelize.Alignment, fillColor, fontColor string) (int, error) {
		st := &excelize.Style{Font: font(14, false, fontColor), Alignment: align, Border: borders(xlLine)}
		if fillColor != "" {
			st.Fill = fill(fillColor)
		}
		if numfmt != "" {
			st.CustomNumFmt = &numfmt
		}
		return mk(st)
	}
	if s.text, err = body("", left, "", xlInk); err != nil {
		return nil, err
	}
	if s.textZebra, err = body("", left, xlZebra, xlInk); err != nil {
		return nil, err
	}
	if s.num, err = body(numFmt, right, "", xlInk); err != nil {
		return nil, err
	}
	if s.numZebra, err = body(numFmt, right, xlZebra, xlInk); err != nil {
		return nil, err
	}
	if s.baht, err = body(bahtFmt, right, "", xlInk); err != nil {
		return nil, err
	}
	if s.bahtZebra, err = body(bahtFmt, right, xlZebra, xlInk); err != nil {
		return nil, err
	}
	if s.pct, err = body(pctFmt, right, "", xlInk); err != nil {
		return nil, err
	}
	if s.pctZebra, err = body(pctFmt, right, xlZebra, xlInk); err != nil {
		return nil, err
	}
	if s.pctWarn, err = body(pctFmt, right, xlWarnSoft, xlWarn); err != nil {
		return nil, err
	}
	if s.pctDanger, err = body(pctFmt, right, xlDangerSoft, xlDanger); err != nil {
		return nil, err
	}
	if s.textWarnTint, err = body("", left, xlWarnSoft, xlInk); err != nil {
		return nil, err
	}
	if s.textDangerTint, err = body("", left, xlDangerSoft, xlInk); err != nil {
		return nil, err
	}
	if s.numWarnTint, err = body(numFmt, right, xlWarnSoft, xlInk); err != nil {
		return nil, err
	}
	if s.numDangerTint, err = body(numFmt, right, xlDangerSoft, xlInk); err != nil {
		return nil, err
	}
	if s.bahtWarnTint, err = body(bahtFmt, right, xlWarnSoft, xlInk); err != nil {
		return nil, err
	}
	if s.bahtDangerTint, err = body(bahtFmt, right, xlDangerSoft, xlInk); err != nil {
		return nil, err
	}
	status := func(fg, bg string) (int, error) {
		return mk(&excelize.Style{Font: font(13, true, fg), Fill: fill(bg), Alignment: center, Border: borders(xlLine)})
	}
	if s.statusOK, err = status(xlGood, xlGoodSoft); err != nil {
		return nil, err
	}
	if s.statusWarn, err = status(xlWarn, xlWarnSoft); err != nil {
		return nil, err
	}
	if s.statusOver, err = status(xlDanger, xlDangerSoft); err != nil {
		return nil, err
	}
	return s, nil
}

const (
	shSummary    = "สรุป"
	shMonthly    = "รายเดือน"
	shCurriculum = "รายหลักสูตร"
	shCourses    = "รายวิชา"
)

// AnalyticsWorkbook renders a TermAnalytics as a styled .xlsx report.
func AnalyticsWorkbook(a *TermAnalytics) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	st, err := buildAnalyticsStyles(f)
	if err != nil {
		return nil, err
	}

	if err := f.SetSheetName("Sheet1", shSummary); err != nil {
		return nil, err
	}
	for _, name := range []string{shMonthly, shCurriculum, shCourses} {
		if _, err := f.NewSheet(name); err != nil {
			return nil, err
		}
	}

	if err := fillMonthlySheet(f, st, a); err != nil {
		return nil, err
	}
	if err := fillCurriculumSheet(f, st, a); err != nil {
		return nil, err
	}
	if err := fillCoursesSheet(f, st, a); err != nil {
		return nil, err
	}
	if err := fillSummarySheet(f, st, a); err != nil {
		return nil, err
	}

	// Open on the cover page.
	idx, err := f.GetSheetIndex(shSummary)
	if err != nil {
		return nil, err
	}
	f.SetActiveSheet(idx)

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func setRow(f *excelize.File, sheet string, row int, style int, vals ...any) error {
	cell := fmt.Sprintf("A%d", row)
	if err := f.SetSheetRow(sheet, cell, &vals); err != nil {
		return err
	}
	end, _ := excelize.ColumnNumberToName(len(vals))
	return f.SetCellStyle(sheet, cell, fmt.Sprintf("%s%d", end, row), style)
}

func fillMonthlySheet(f *excelize.File, st *analyticsStyles, a *TermAnalytics) error {
	if err := setRow(f, shMonthly, 1, st.header, "เดือน", "ยอดเบิกจ่าย (บาท)", "ยอดสะสม (บาท)"); err != nil {
		return err
	}
	cum := 0.0
	for i, m := range a.Monthly {
		cum += m.Baht
		row := i + 2
		if err := setRow(f, shMonthly, row, st.baht, thaiMonthShortBE(m.YearMonth), m.Baht, round2(cum)); err != nil {
			return err
		}
		// The month label is text, not money.
		style := st.text
		if err := f.SetCellStyle(shMonthly, fmt.Sprintf("A%d", row), fmt.Sprintf("A%d", row), style); err != nil {
			return err
		}
	}
	_ = f.SetColWidth(shMonthly, "A", "A", 14)
	_ = f.SetColWidth(shMonthly, "B", "C", 20)
	return f.SetPanes(shMonthly, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
}

func fillCurriculumSheet(f *excelize.File, st *analyticsStyles, a *TermAnalytics) error {
	if err := setRow(f, shCurriculum, 1, st.header,
		"หลักสูตร", "วิชาที่เปิด", "วิชาที่ใช้ TA", "จำนวน TA", "ยอดเบิกจ่าย (บาท)", "เพดานงบ (บาท)", "% ของเพดาน"); err != nil {
		return err
	}
	for i, cu := range a.Curricula {
		row := i + 2
		var pct any
		if cu.CapBaht > 0 {
			pct = cu.SpentBaht / cu.CapBaht
		} else {
			pct = ""
		}
		if err := setRow(f, shCurriculum, row, st.text,
			CurriculumTH(cu.Curriculum), cu.CoursesOpen, cu.CoursesWithTA, cu.TAs, cu.SpentBaht, cu.CapBaht, pct); err != nil {
			return err
		}
		zebra := i%2 == 1
		numSt, bahtSt, pctSt, textSt := st.num, st.baht, st.pct, st.text
		if zebra {
			numSt, bahtSt, pctSt, textSt = st.numZebra, st.bahtZebra, st.pctZebra, st.textZebra
		}
		r := strconv.Itoa(row)
		_ = f.SetCellStyle(shCurriculum, "A"+r, "A"+r, textSt)
		_ = f.SetCellStyle(shCurriculum, "B"+r, "D"+r, numSt)
		_ = f.SetCellStyle(shCurriculum, "E"+r, "F"+r, bahtSt)
		_ = f.SetCellStyle(shCurriculum, "G"+r, "G"+r, pctSt)
	}
	_ = f.SetColWidth(shCurriculum, "A", "A", 30)
	_ = f.SetColWidth(shCurriculum, "B", "D", 13)
	_ = f.SetColWidth(shCurriculum, "E", "F", 19)
	_ = f.SetColWidth(shCurriculum, "G", "G", 13)
	return f.SetPanes(shCurriculum, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
}

func fillCoursesSheet(f *excelize.File, st *analyticsStyles, a *TermAnalytics) error {
	if err := setRow(f, shCourses, 1, st.header,
		"รหัสวิชา", "ชื่อวิชา", "หลักสูตร", "จำนวน TA", "ชม.อนุมัติ", "ยอดเบิกจ่าย (บาท)", "เพดานวิชา (บาท)", "% ของเพดาน", "สถานะ"); err != nil {
		return err
	}
	for i, co := range a.Courses {
		row := i + 2
		var pct any
		pctVal := 0.0
		if co.CapBaht > 0 {
			pctVal = co.SpentBaht / co.CapBaht
			pct = pctVal
		} else {
			pct = ""
		}
		statusTxt, statusSt := "ปกติ", st.statusOK
		textSt, numSt, bahtSt, pctSt := st.text, st.num, st.baht, st.pct
		if i%2 == 1 {
			textSt, numSt, bahtSt, pctSt = st.textZebra, st.numZebra, st.bahtZebra, st.pctZebra
		}
		switch {
		case co.OverBudget:
			statusTxt, statusSt = "เกินเพดาน", st.statusOver
			textSt, numSt, bahtSt, pctSt = st.textDangerTint, st.numDangerTint, st.bahtDangerTint, st.pctDanger
		case pctVal >= 0.8:
			statusTxt, statusSt = "ใกล้เพดาน", st.statusWarn
			textSt, numSt, bahtSt, pctSt = st.textWarnTint, st.numWarnTint, st.bahtWarnTint, st.pctWarn
		case co.SpentBaht <= 0:
			statusTxt = "ยังไม่เริ่มเบิก"
		}
		if err := setRow(f, shCourses, row, textSt,
			co.Code, co.NameTH, CurriculumTH(co.Curriculum), co.TAs, co.ApprovedHours, co.SpentBaht, co.CapBaht, pct, statusTxt); err != nil {
			return err
		}
		r := strconv.Itoa(row)
		_ = f.SetCellStyle(shCourses, "D"+r, "E"+r, numSt)
		_ = f.SetCellStyle(shCourses, "F"+r, "G"+r, bahtSt)
		_ = f.SetCellStyle(shCourses, "H"+r, "H"+r, pctSt)
		_ = f.SetCellStyle(shCourses, "I"+r, "I"+r, statusSt)
	}
	_ = f.SetColWidth(shCourses, "A", "A", 13)
	_ = f.SetColWidth(shCourses, "B", "B", 52)
	_ = f.SetColWidth(shCourses, "C", "C", 22)
	_ = f.SetColWidth(shCourses, "D", "E", 12)
	_ = f.SetColWidth(shCourses, "F", "G", 18)
	_ = f.SetColWidth(shCourses, "H", "H", 12)
	_ = f.SetColWidth(shCourses, "I", "I", 24)
	return f.SetPanes(shCourses, &excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"})
}

func fillSummarySheet(f *excelize.File, st *analyticsStyles, a *TermAnalytics) error {
	// Title band.
	if err := f.MergeCell(shSummary, "A1", "H2"); err != nil {
		return err
	}
	if err := f.SetCellValue(shSummary, "A1", "รายงานการใช้งบประมาณผู้ช่วยสอน (TA)"); err != nil {
		return err
	}
	_ = f.SetCellStyle(shSummary, "A1", "H2", st.title)
	_ = f.MergeCell(shSummary, "A3", "H3")
	sub := "ยอดเบิกจ่ายจริง ตรงกับเอกสารเบิกจ่าย"
	if a.TermLabel != "" {
		sub = "ปีการศึกษา " + a.TermLabel + " · " + sub
	}
	_ = f.SetCellValue(shSummary, "A3", sub)
	_ = f.SetCellStyle(shSummary, "A3", "H3", st.subtitle)
	_ = f.SetRowHeight(shSummary, 1, 28)
	_ = f.SetRowHeight(shSummary, 2, 14)
	_ = f.SetRowHeight(shSummary, 3, 22)

	// KPI block, two columns of label/value pairs.
	remaining := a.BudgetAllocated - a.BudgetUsed
	if remaining < 0 {
		remaining = 0
	}
	usedPct := 0.0
	if a.BudgetAllocated > 0 {
		usedPct = a.BudgetUsed / a.BudgetAllocated
	}
	avg := 0.0
	if a.ApprovedHours > 0 {
		avg = round2(a.BudgetUsed / a.ApprovedHours)
	}
	valueStyle := st.kpiValue
	if usedPct >= 0.8 {
		valueStyle = st.kpiValueWarn
	}
	type kpi struct {
		label string
		value any
		style int
	}
	leftCol := []kpi{
		{"เบิกจ่ายแล้ว (บาท)", a.BudgetUsed, valueStyle},
		{"เพดานงบรวม (บาท)", a.BudgetAllocated, st.kpiValue},
		{"คงเหลือ (บาท)", round2(remaining), st.kpiValueGood},
		{"ใช้ไปแล้ว (%)", usedPct, valueStyle},
	}
	rightCol := []kpi{
		{"ชั่วโมงที่อนุมัติแล้ว", a.ApprovedHours, st.kpiValue},
		{"ค่าใช้จ่ายเฉลี่ยต่อชั่วโมง (บาท)", avg, st.kpiValue},
		{"TA ที่ปฏิบัติงาน (คน)", a.TotalTAs, st.kpiValue},
		{"วิชาที่ใช้ TA / เปิดสอน", fmt.Sprintf("%d / %d", a.CoursesWithTA, a.CoursesOpen), st.kpiValue},
	}
	for i, k := range leftCol {
		row := strconv.Itoa(5 + i)
		_ = f.SetCellValue(shSummary, "A"+row, k.label)
		_ = f.SetCellStyle(shSummary, "A"+row, "B"+row, st.kpiLabel)
		_ = f.MergeCell(shSummary, "A"+row, "B"+row)
		_ = f.SetCellValue(shSummary, "C"+row, k.value)
		_ = f.SetCellStyle(shSummary, "C"+row, "C"+row, k.style)
	}
	// The percentage cell needs its own format.
	pctStyle, err := f.NewStyle(&excelize.Style{
		Font:         &excelize.Font{Family: "TH Sarabun New", Size: 20, Bold: true, Color: ifStr(usedPct >= 0.8, xlWarn, xlBrand)},
		Alignment:    &excelize.Alignment{Horizontal: "right", Vertical: "center"},
		CustomNumFmt: &[]string{"0.0%"}[0],
	})
	if err != nil {
		return err
	}
	_ = f.SetCellStyle(shSummary, "C8", "C8", pctStyle)
	for i, k := range rightCol {
		row := strconv.Itoa(5 + i)
		_ = f.SetCellValue(shSummary, "E"+row, k.label)
		_ = f.SetCellStyle(shSummary, "E"+row, "F"+row, st.kpiLabel)
		_ = f.MergeCell(shSummary, "E"+row, "F"+row)
		_ = f.SetCellValue(shSummary, "G"+row, k.value)
		_ = f.SetCellStyle(shSummary, "G"+row, "G"+row, k.style)
	}
	_ = f.SetCellValue(shSummary, "A10",
		"ข้อมูลจากระบบ TA Payment · วิชาที่ใช้เกินงบจะเบิกได้ตามเพดานที่กำหนดเท่านั้น")
	_ = f.SetCellStyle(shSummary, "A10", "H10", st.kpiHint)
	_ = f.MergeCell(shSummary, "A10", "H10")

	for col, w := range map[string]float64{"A": 16, "B": 16, "C": 17, "D": 4, "E": 16, "F": 16, "G": 17, "H": 10} {
		_ = f.SetColWidth(shSummary, col, col, w)
	}

	// Charts — native Excel objects fed by the data sheets, so a reader can
	// restyle or extend them without leaving Excel.
	n := len(a.Monthly)
	if n > 0 {
		rng := func(col string) string {
			return fmt.Sprintf("%s!$%s$2:$%s$%d", shMonthly, col, col, n+1)
		}
		bars := excelize.Chart{
			Type: excelize.Col,
			Series: []excelize.ChartSeries{{
				Name:       shMonthly + "!$B$1",
				Categories: rng("A"),
				Values:     rng("B"),
				Fill:       excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{xlBrand}},
			}},
			Title:     excelize.ChartTitle{Paragraph: []excelize.RichTextRun{{Text: "การเบิกจ่ายรายเดือน (บาท)"}}},
			Dimension: excelize.ChartDimension{Width: 460, Height: 300},
			Legend:    excelize.ChartLegend{Position: "bottom"},
			PlotArea:  excelize.ChartPlotArea{ShowVal: true},
		}
		cumulative := excelize.Chart{
			Type: excelize.Line,
			Series: []excelize.ChartSeries{{
				Name:       shMonthly + "!$C$1",
				Categories: rng("A"),
				Values:     rng("C"),
				Fill:       excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{xlGood}},
			}},
			Legend: excelize.ChartLegend{Position: "bottom"},
		}
		if err := f.AddChart(shSummary, "A12", &bars, &cumulative); err != nil {
			return err
		}
	}
	if len(a.Curricula) > 0 {
		n := len(a.Curricula)
		rng := func(col string) string {
			return fmt.Sprintf("%s!$%s$2:$%s$%d", shCurriculum, col, col, n+1)
		}
		bar := excelize.Chart{
			Type: excelize.Bar,
			Series: []excelize.ChartSeries{{
				Name:       shCurriculum + "!$E$1",
				Categories: rng("A"),
				Values:     rng("E"),
				Fill:       excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{xlBrand}},
			}},
			Title:     excelize.ChartTitle{Paragraph: []excelize.RichTextRun{{Text: "ยอดเบิกจ่ายรายหลักสูตร (บาท)"}}},
			Dimension: excelize.ChartDimension{Width: 460, Height: 300},
			Legend:    excelize.ChartLegend{Position: "bottom"},
			PlotArea:  excelize.ChartPlotArea{ShowVal: true},
		}
		if err := f.AddChart(shSummary, "E12", &bar); err != nil {
			return err
		}
	}
	return nil
}

// ifStr is a tiny ternary for style colours.
func ifStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
