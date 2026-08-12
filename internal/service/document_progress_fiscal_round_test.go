package service

import (
	"testing"

	"github.com/google/uuid"
)

/* -------------------------------------------------------------------------- */
/* Fiscal-round document progress (12/08/2026)                                */
/*                                                                            */
/* งบแผ่นดิน closes 30 กันยายน but ภาคต้น teaches มิ.ย.–ต.ค., so staff sign and  */
/* route TWO physical documents for one term. document_progress and          */
/* signature_checklist gained fiscal_round so the two journeys never collide  */
/* — and a term that never crosses the boundary (the ordinary case) must     */
/* behave EXACTLY as it always did, with no "round" language anywhere.       */
/* -------------------------------------------------------------------------- */

func progressSvcTC(f *tcFixture) *DocumentProgressService {
	return &DocumentProgressService{pool: f.pool, aud: f.svc.aud, export: f.svc}
}

// recordExportBatch simulates BuildCourseZip's side effect on export_batches
// — the ledger round2ExportedSQL actually reads — without needing the full
// ZIP-building machinery (docs, storage, PII) a real export goes through.
func (f *tcFixture) recordExportBatch(courseID, generatedBy uuid.UUID, months []string) {
	f.t.Helper()
	f.exec(`INSERT INTO export_batches
	            (id, teaching_course_id, file_path, file_name, ta_count, total_baht, generated_at, generated_by, months)
	        VALUES (gen_random_uuid(), $1, 'x', 'x.zip', 0, 0, now(), $2, $3)`,
		courseID, generatedBy, months)
}

// A term whose submission_periods never reach ตุลาคม (or any month past the
// budget-year boundary) does not cross it — GetOverview must return exactly
// ONE round, with no label, indistinguishable from the board before fiscal
// rounds existed.
func TestGetOverview_NonCrossingTermHasExactlyOneUnlabeledRound(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP510", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("รอบเดียว ทดสอบ", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-08-10"})

	svc := progressSvcTC(f)
	overview, err := svc.GetOverview(f.ctx, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Rounds) != 1 {
		t.Fatalf("rounds = %d, want 1 — this term never crosses the budget year", len(overview.Rounds))
	}
	r := overview.Rounds[0]
	if r.Round != 1 {
		t.Errorf("round = %d, want 1", r.Round)
	}
	if r.RoundLabel != "" {
		t.Errorf("round_label = %q, want empty — a single-round term must show no round language", r.RoundLabel)
	}
}

// Crossing the budget year but having NOTHING billable after it (no work_logs,
// no grad-special TA at all) must still collapse to one round — the whole
// point of gating round 2 on actual content, per the 12/08/2026 decision that
// an empty round should not exist rather than sit at 0/0 forever.
func TestGetOverview_CrossingTermWithNoRound2ContentStaysOneRound(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP520", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("งบไม่คร่อม ทดสอบ", "undergrad")
	// Work logged only up to กันยายน — nothing crosses into ตุลาคม.
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-08-10", "2026-09-14"})

	svc := progressSvcTC(f)
	overview, err := svc.GetOverview(f.ctx, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Rounds) != 1 {
		t.Fatalf("rounds = %d, want 1 — round 2 has no billable content", len(overview.Rounds))
	}
}

// The core case: a term that crosses the boundary AND has real ตุลาคม work
// gets a genuine second round, independent of the first — different course
// total, different export state, different label.
func TestGetOverview_CrossingTermWithRound2ContentHasTwoIndependentRounds(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP530", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("ต่อเนื่อง ทดสอบ", "undergrad")
	// A continuing TA: work both sides of 30 กันยายน.
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-08-10", "2026-10-05"})

	svc := progressSvcTC(f)
	overview, err := svc.GetOverview(f.ctx, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Rounds) != 2 {
		t.Fatalf("rounds = %d, want 2 — the term crosses AND ตุลาคม has real work", len(overview.Rounds))
	}
	r1, r2 := overview.Rounds[0], overview.Rounds[1]
	if r1.Round != 1 || r2.Round != 2 {
		t.Fatalf("round numbers = %d, %d, want 1, 2", r1.Round, r2.Round)
	}
	if r1.RoundLabel == "" || r2.RoundLabel == "" {
		t.Error("both round labels must be populated once the term has two rounds")
	}
	if r1.RoundLabel == r2.RoundLabel {
		t.Errorf("round labels must differ, got the same for both: %q", r1.RoundLabel)
	}

	// Round 1 uses the ORIGINAL exported_at flag — untouched by this feature.
	if r1.TotalCourses != 1 || r1.ExportedCourses != 0 {
		t.Errorf("round 1 readiness = %d/%d, want 1/0 before any export", r1.TotalCourses, r1.ExportedCourses)
	}
	f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE id = $1`, courseID)
	overview, err = svc.GetOverview(f.ctx, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	r1, r2 = overview.Rounds[0], overview.Rounds[1]
	if !r1.AllExported {
		t.Error("round 1 must read AllExported once exported_at is set — unchanged semantics")
	}
	// Round 1's export must NOT satisfy round 2 — they are separate documents.
	if r2.AllExported {
		t.Error("round 1's export must not leak into round 2's readiness")
	}
	if r2.TotalCourses != 1 || r2.ExportedCourses != 0 {
		t.Errorf("round 2 readiness = %d/%d, want 1/0 — no export_batches row for ตุลาคม yet", r2.TotalCourses, r2.ExportedCourses)
	}

	// Now actually "export" round 2's slice.
	f.recordExportBatch(courseID, f.lectID, []string{"2026-10"})
	overview, err = svc.GetOverview(f.ctx, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	r2 = overview.Rounds[1]
	if !r2.AllExported {
		t.Error("round 2 must read AllExported once an export_batches row covers ตุลาคม")
	}
}

// ListChecklist(round=2) must include ONLY courses with demonstrable round-2
// content — a course whose TA never worked past กันยายน has no round-2
// document at all, and must not appear asking for a signature on one.
func TestListChecklist_Round2OnlyIncludesCoursesWithRound2Content(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseA, regA, _ := f.insertCourse(tcCourseOpts{Code: "CP540", Curriculum: "CY", LectureHrs: 100})
	taA := f.newTA("มีงานตุลา ทดสอบ", "undergrad")
	f.assignTAOn(taA, courseA, regA, "undergrad", []string{"2026-08-10", "2026-10-05"})

	courseB, regB, _ := f.insertCourse(tcCourseOpts{Code: "CP541", Curriculum: "CY", LectureHrs: 100})
	taB := f.newTA("ไม่มีงานตุลา ทดสอบ", "undergrad")
	f.assignTAOn(taB, courseB, regB, "undergrad", []string{"2026-08-11"})

	svc := progressSvcTC(f)
	round1, err := svc.ListChecklist(f.ctx, f.termID, 1)
	if err != nil {
		t.Fatal(err)
	}
	round2, err := svc.ListChecklist(f.ctx, f.termID, 2)
	if err != nil {
		t.Fatal(err)
	}

	codes1 := map[string]bool{}
	for _, it := range round1 {
		codes1[it.Code] = true
	}
	if !codes1["CP540"] || !codes1["CP541"] {
		t.Errorf("round 1 must list BOTH courses (roster is fixed at appointment) — got %v", codes1)
	}

	codes2 := map[string]bool{}
	for _, it := range round2 {
		codes2[it.Code] = true
	}
	if !codes2["CP540"] {
		t.Error("round 2 must list CP540 — its TA worked in ตุลาคม")
	}
	if codes2["CP541"] {
		t.Error("round 2 must NOT list CP541 — its TA never worked past กันยายน, so there is no round-2 document to sign")
	}
}

// The two rounds' signatures must never collide: signing round 1's document
// must not be readable as round 2's signature for the same person, course,
// and role — and vice versa. This is the exact bug fiscal_round exists to
// prevent (before it, there was only one signature_checklist row per
// course-role-signer, so a second round had nowhere of its own to write).
func TestToggleSignature_RoundsAreIndependentForTheSamePerson(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP550", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("เซ็นสองรอบ ทดสอบ", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-08-10", "2026-10-05"})
	staff := f.actor()

	svc := progressSvcTC(f)
	if err := svc.ToggleSignature(f.ctx, staff, courseID, "ta", &ta, true, 1); err != nil {
		t.Fatal(err)
	}

	round1, err := svc.ListChecklist(f.ctx, f.termID, 1)
	if err != nil {
		t.Fatal(err)
	}
	round2, err := svc.ListChecklist(f.ctx, f.termID, 2)
	if err != nil {
		t.Fatal(err)
	}
	signedIn := func(items []SignatureItem) *string {
		for _, it := range items {
			if it.Role == "ta" && it.SignerID != nil && *it.SignerID == ta {
				return it.SignedAt
			}
		}
		return nil
	}
	if signedIn(round1) == nil {
		t.Fatal("round 1's own tick must show the TA as signed")
	}
	if signedIn(round2) != nil {
		t.Fatal("round 1's signature must NOT satisfy round 2 — they are separate physical documents")
	}

	if err := svc.ToggleSignature(f.ctx, staff, courseID, "ta", &ta, true, 2); err != nil {
		t.Fatal(err)
	}
	round2, err = svc.ListChecklist(f.ctx, f.termID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if signedIn(round2) == nil {
		t.Fatal("round 2's own tick must show the TA as signed once toggled for round 2")
	}
}

// SetStage on one round must never move the other round's stage, and a term
// with no round 2 must refuse SetStage(round=2) outright rather than silently
// creating a document_progress row nobody will ever see on screen.
func TestSetStage_RoundsAdvanceIndependently(t *testing.T) {
	f := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07", "2569-08", "2569-09", "2569-10"} {
		f.addPeriod(ym)
	}
	courseID, regSec, _ := f.insertCourse(tcCourseOpts{Code: "CP560", Curriculum: "CY", LectureHrs: 100})
	ta := f.newTA("สองรอบ ก้าวหน้า", "undergrad")
	f.assignTAOn(ta, courseID, regSec, "undergrad", []string{"2026-08-10", "2026-10-05"})
	lect := f.lectID
	staff := f.actor()
	svc := progressSvcTC(f)

	// Get both rounds export-ready.
	f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE id = $1`, courseID)
	f.recordExportBatch(courseID, staff, []string{"2026-10"})

	// Sign round 1 fully and advance it to stage 2.
	if err := svc.ToggleSignature(f.ctx, staff, courseID, "ta", &ta, true, 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.ToggleSignature(f.ctx, staff, courseID, "lecturer", &lect, true, 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStage(f.ctx, staff, f.termID, 2, "", 1); err != nil {
		t.Fatalf("round 1 stage 2: %v", err)
	}

	// Round 2 has NO signatures yet — stage 1 must still be refused.
	if err := svc.SetStage(f.ctx, staff, f.termID, 1, "", 2); err == nil {
		t.Fatal("round 2 stage 1 must be refused — its own TA has not signed round 2's document")
	}

	overview, err := svc.GetOverview(f.ctx, f.termID)
	if err != nil {
		t.Fatal(err)
	}
	r1, r2 := overview.Rounds[0], overview.Rounds[1]
	if r1.Stage != 2 {
		t.Errorf("round 1 stage = %d, want 2", r1.Stage)
	}
	if r2.Stage != 0 {
		t.Errorf("round 2 stage = %d, want 0 — advancing round 1 must not move round 2", r2.Stage)
	}

	// A term whose round 2 does not exist must refuse SetStage(round=2)
	// outright — proven against a second, non-crossing fixture.
	g := newTCFixture(t)
	for _, ym := range []string{"2569-06", "2569-07"} {
		g.addPeriod(ym)
	}
	gCourse, gReg, _ := g.insertCourse(tcCourseOpts{Code: "CP561", Curriculum: "CY", LectureHrs: 100})
	gTA := g.newTA("ไม่มีรอบสอง ทดสอบ", "undergrad")
	g.assignTAOn(gTA, gCourse, gReg, "undergrad", []string{"2026-06-10"})
	gStaff := g.actor()
	gSvc := progressSvcTC(g)
	if err := gSvc.SetStage(g.ctx, gStaff, g.termID, 1, "", 2); err == nil {
		t.Error("SetStage(round=2) on a term with no round 2 must be refused")
	}
}
