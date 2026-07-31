package pdfgen

import (
	"bytes"
	"strings"
	"testing"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfmodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// pdfcpu panics on a nil configuration; every call here supplies the default.
func pdfConf() *pdfmodel.Configuration { return pdfmodel.NewDefaultConfiguration() }

// The PDF renderer exists so the timetable form can travel in the payout zip,
// where no browser is available. These tests hold the two properties the paper
// document depends on and the geometry that has already gone wrong once.

func sampleForm() TimetableFormData {
	return TimetableFormData{
		TAName:    "นายจิรายุ ร่มลำดวน",
		StudentID: "673380499-3",
		TermLabel: "2569/1",
		YearMonth: "2569-07",
		Blocks: []TimetableFormBlock{
			{Kind: "own_class", CourseCode: "SC363762", DayOfWeek: 1, StartTime: "09:00", EndTime: "12:00"},
			{Kind: "lab", CourseCode: "CP321002", SecNo: "1", Track: "regular",
				DayOfWeek: 1, StartTime: "15:00", EndTime: "17:00", Expected: 4, Logged: 4},
			// Evening grading — the column that did not exist until 31/07/2026.
			{Kind: "review", CourseCode: "CP321002", SecNo: "2", Track: "special",
				DayOfWeek: 1, StartTime: "20:00", EndTime: "21:00"},
		},
		Signers: []TimetableFormSigner{
			{LecturerName: "ผศ.ดร.วรัญญา วรรณศรี", Courses: []string{"CP351203", "CP321002"}},
		},
		OutOfGrid: []TimetableFormOutOfGrid{
			{Date: "2569-07-25", Start: "13:00", End: "15:00", Kind: "ปฏิบัติการ",
				Course: "CP351203", SecNo: "1", Note: "สอนปฏิบัติ (ชดเชย)", Source: "auto"},
		},
	}
}

func render(t *testing.T, d TimetableFormData) []byte {
	t.Helper()
	out, err := BuildTimetableFormPDF(TimetableFormInput{FontDir: "../../assets/fonts", Data: d})
	if err != nil {
		t.Fatalf("BuildTimetableFormPDF: %v", err)
	}
	return out
}

func TestTimetableFormPDF_RendersAValidDocument(t *testing.T) {
	out := render(t, sampleForm())
	if !bytes.HasPrefix(out, []byte("%PDF")) {
		t.Fatalf("output is not a PDF (first bytes: %q)", out[:min(8, len(out))])
	}
	if err := pdfapi.Validate(bytes.NewReader(out), pdfConf()); err != nil {
		t.Fatalf("pdfcpu rejects the output: %v", err)
	}
}

// The form is a wide hourly grid; portrait would squeeze thirteen columns into
// 595pt and make the labels unreadable.
func TestTimetableFormPDF_IsA4Landscape(t *testing.T) {
	out := render(t, sampleForm())
	dims, err := pdfapi.PageDims(bytes.NewReader(out), pdfConf())
	if err != nil {
		t.Fatalf("PageDims: %v", err)
	}
	if len(dims) != 1 {
		t.Fatalf("want a single page, got %d", len(dims))
	}
	if dims[0].Width < dims[0].Height {
		t.Errorf("page is %.0f×%.0f — the grid needs landscape", dims[0].Width, dims[0].Height)
	}
}

// ── geometry ────────────────────────────────────────────────────────────────

// The bug that started this: the grid ended at 20.00, so `span`-style clamping
// collapsed a 20.00–21.00 duty to zero width and it disappeared with nothing
// said. The PDF renderer must not repeat it.
func TestTimetableClamp_KeepsTheEveningColumn(t *testing.T) {
	s, e := ttClamp(ttMin("20:00")), ttClamp(ttMin("21:00"))
	if e <= s {
		t.Fatalf("20:00–21:00 collapsed to zero width (start=%d end=%d) — "+
			"an evening grading slot would vanish from the signed form", s, e)
	}
	if got := e - s; got != 60 {
		t.Errorf("width = %d minutes, want 60", got)
	}
}

func TestTimetableClamp_PinsBlocksOutsideTheWindow(t *testing.T) {
	// Wholly after the grid: must collapse, so the caller can notice and skip.
	if ttClamp(ttMin("21:00")) != ttClamp(ttMin("22:00")) {
		t.Error("a 21:00–22:00 block should clamp to zero width, not draw off-page")
	}
	// Partially before: keeps the drawable part.
	if got := ttClamp(ttMin("07:00")); got != ttFirstHour*60 {
		t.Errorf("07:00 clamped to %d, want %d", got, ttFirstHour*60)
	}
}

// Co-taught sections meet at the same hour by design (sec 1 ภาคปกติ + sec 2
// โครงการพิเศษ in one room). Drawn on one line they would overwrite each other
// and the form would show one duty where there are two.
func TestTimetableLanes_StacksOverlappingBlocks(t *testing.T) {
	rows := ttLanes([]TimetableFormBlock{
		{CourseCode: "A", StartTime: "13:00", EndTime: "15:00"},
		{CourseCode: "B", StartTime: "13:00", EndTime: "15:00"},
	})
	if len(rows) != 2 {
		t.Fatalf("two co-taught sections packed into %d sub-row(s) — "+
			"they would be drawn on top of each other", len(rows))
	}
}

// ...but blocks that merely sit next to each other must share a line, or every
// day grows a row per session and the page stops fitting.
func TestTimetableLanes_SharesOneRowWhenTheyDoNotOverlap(t *testing.T) {
	rows := ttLanes([]TimetableFormBlock{
		{CourseCode: "A", StartTime: "09:00", EndTime: "11:00"},
		{CourseCode: "B", StartTime: "11:00", EndTime: "13:00"}, // touching, not overlapping
		{CourseCode: "C", StartTime: "14:00", EndTime: "15:00"},
	})
	if len(rows) != 1 {
		t.Errorf("three non-overlapping blocks used %d rows, want 1", len(rows))
	}
}

func TestTimetablePick_SplitsLanesAndDropsZeroLengthRows(t *testing.T) {
	blocks := []TimetableFormBlock{
		{Kind: "own_class", DayOfWeek: 1, StartTime: "09:00", EndTime: "12:00"},
		{Kind: "lab", DayOfWeek: 1, StartTime: "13:00", EndTime: "16:00"},
		{Kind: "lab", DayOfWeek: 2, StartTime: "13:00", EndTime: "16:00"}, // other day
		{Kind: "lab", DayOfWeek: 1, StartTime: "10:00", EndTime: "10:00"}, // malformed
	}
	if got := len(ttPick(blocks, 1, true)); got != 1 {
		t.Errorf("own lane = %d blocks, want 1", got)
	}
	duty := ttPick(blocks, 1, false)
	if len(duty) != 1 {
		t.Errorf("duty lane = %d blocks, want 1 (the zero-length row must be dropped, "+
			"not drawn as an invisible sliver)", len(duty))
	}
}

// The label is what survives photocopying, so it must name the section and the
// kind — colour alone does not reach a black-and-white signed copy.
func TestTimetableLabel_NamesSectionKindAndTrack(t *testing.T) {
	got := ttLabel(TimetableFormBlock{
		Kind: "lab", CourseCode: "CP351203", SecNo: "2", Track: "special",
		Expected: 4, Logged: 3,
	})
	for _, want := range []string{"CP351203", "Sec.2", "ปฏิบัติการ", "พิเศษ", "3/4"} {
		if !strings.Contains(got, want) {
			t.Errorf("label %q is missing %q", got, want)
		}
	}
}

// A TA with no duties at all still gets a form — a blank one to fill in by hand
// beats no document when the office expects one in the folder.
func TestTimetableFormPDF_EmptyFormStillRenders(t *testing.T) {
	out := render(t, TimetableFormData{TAName: "ทดสอบ", TermLabel: "2569/1"})
	if err := pdfapi.Validate(bytes.NewReader(out), pdfConf()); err != nil {
		t.Fatalf("an empty form must still produce a valid PDF: %v", err)
	}
}
