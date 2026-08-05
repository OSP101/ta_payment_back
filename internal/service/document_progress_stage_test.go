package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The board is a sequence, not five free buttons. The paper moves one desk at a
// time, so a stage can only be recorded once the one before it is fully signed.
// Until 04/08/2026 staff could click any circle, so a term could read
// "ส่งการเงินแล้ว" while half the TAs had never signed.

func progressSvc(f *fixture) *DocumentProgressService {
	return &DocumentProgressService{pool: f.Pool, aud: f.Svc.aud, export: exportSvcFor(f)}
}

// exportedTerm gets the fixture past the export gate, which is checked before
// any of the signature rules.
func exportedTerm(f *fixture) {
	f.exec(`UPDATE teaching_courses SET exported_at = now() WHERE term_id = $1`, f.TermID)
}

func TestSetStage_RefusesToSkipPastUnsignedTAs(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	exportedTerm(f)
	svc := progressSvc(f)

	err := svc.SetStage(f.ctx, f.StaffID, f.TermID, 2, "")
	if err == nil {
		t.Fatal("stage 2 must be refused while the TA has not signed")
	}
	if !strings.Contains(err.Error(), "TA เซ็น") {
		t.Errorf("error must name the stage that is holding it up, got: %v", err)
	}
	// And it must name the person, or the officer has nobody to call.
	if !strings.Contains(err.Error(), "ta Test") {
		t.Errorf("error must name who is missing, got: %v", err)
	}
}

func TestSetStage_AllowsTheNextStageOnceEverySignatureIsIn(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	exportedTerm(f)
	svc := progressSvc(f)

	// Pressing "TA เซ็นครบ" IS the claim that they all signed, so it needs their
	// signatures — being exported is not enough.
	if err := svc.SetStage(f.ctx, f.StaffID, f.TermID, 1, ""); err == nil {
		t.Fatal("stage 1 must be refused while a TA has not signed")
	}
	ta := f.TAID
	if err := svc.ToggleSignature(f.ctx, f.StaffID, f.CourseID, "ta", &ta, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStage(f.ctx, f.StaffID, f.TermID, 1, ""); err != nil {
		t.Fatalf("stage 1 must be allowed once every TA signed: %v", err)
	}
	// Stage 2 now needs the lecturer's tick on top.
	if err := svc.SetStage(f.ctx, f.StaffID, f.TermID, 2, ""); err == nil {
		t.Fatal("stage 2 must be refused — the lecturer has not signed")
	}
	lect := f.LecturerID
	if err := svc.ToggleSignature(f.ctx, f.StaffID, f.CourseID, "lecturer", &lect, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStage(f.ctx, f.StaffID, f.TermID, 2, ""); err != nil {
		t.Errorf("stage 2 must be allowed once the lecturer signed: %v", err)
	}
}

// Going backwards is how staff fix a mistake, so it is never gated.
func TestSetStage_BackwardsIsAlwaysAllowed(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	exportedTerm(f)
	svc := progressSvc(f)
	ta, lect := f.TAID, f.LecturerID
	if err := svc.ToggleSignature(f.ctx, f.StaffID, f.CourseID, "ta", &ta, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.ToggleSignature(f.ctx, f.StaffID, f.CourseID, "lecturer", &lect, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStage(f.ctx, f.StaffID, f.TermID, 2, ""); err != nil {
		t.Fatal(err)
	}
	// Un-tick the TA, then reverse — the correction must not be blocked by the
	// very signature the officer is correcting.
	if err := svc.ToggleSignature(f.ctx, f.StaffID, f.CourseID, "ta", &ta, false); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStage(f.ctx, f.StaffID, f.TermID, 1, ""); err != nil {
		t.Errorf("stepping back must always work: %v", err)
	}
}

func TestListChecklist_OneRowPerPerson(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	second := f.insertUser("ta", "ta2")
	reqID := uuid.New()
	f.exec(`INSERT INTO ta_requests (id, teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at)
	        VALUES ($1,$2,$3,'both','approved',now(),now())`, reqID, f.CourseID, f.LecturerID)
	f.exec(`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level)
	        VALUES (gen_random_uuid(), $1, $2, $3, 'undergrad')`, reqID, f.SectionID, second)

	items, err := progressSvc(f).ListChecklist(f.ctx, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	var tas, lecturers, certifiers int
	for _, it := range items {
		switch it.Role {
		case "ta":
			tas++
			if it.SignerID == nil {
				t.Error("a TA row must name the person — the lump is what this replaced")
			}
		case "lecturer":
			lecturers++
		case "certifier":
			certifiers++
			if it.SignerID != nil {
				t.Error("the certifier is one officer per course, not a per-person row")
			}
		}
	}
	if tas != 2 {
		t.Errorf("ta rows = %d, want 2 — one per TA on the course", tas)
	}
	if lecturers != 1 {
		t.Errorf("lecturer rows = %d, want 1 — only whoever submitted the request", lecturers)
	}
	if certifiers != 1 {
		t.Errorf("certifier rows = %d, want 1", certifiers)
	}
}

// The lecturer stage belongs to the submitter. A co-teacher listed on the course
// who filed nothing has nothing to sign, and naming them sent the officer after
// the wrong person.
func TestListChecklist_LecturerIsTheSubmitterNotEveryCoTeacher(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := f.insertUser("lecturer", "co")
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary)
	        VALUES ($1, $2, false)`, f.CourseID, other)

	items, err := progressSvc(f).ListChecklist(f.ctx, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range items {
		if it.Role == "lecturer" {
			names = append(names, it.Responsible)
			if it.SignerID != nil && *it.SignerID == other {
				t.Error("a co-teacher who submitted nothing must not be asked to sign")
			}
		}
	}
	if len(names) != 1 {
		t.Errorf("lecturer rows = %v, want exactly the submitter", names)
	}
}

// The reminder must chase exactly the people the checklist lists. It used to
// join teaching_lecturers and mail every co-teacher on the roster — including
// ones with nothing on the claim to sign — so the button nagged people the
// board never asked for.
func TestRemindUnsigned_TargetsTheSameLecturersTheChecklistDoes(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	co := f.insertUser("lecturer", "co")
	f.exec(`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary)
	        VALUES ($1, $2, false)`, f.CourseID, co)
	svc := progressSvc(f)
	svc.notify = f.Svc.notify

	n, err := svc.RemindUnsigned(f.ctx, f.StaffID, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	items, err := svc.ListChecklist(f.ctx, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, it := range items {
		if it.Role == "lecturer" && it.SignedAt == nil {
			want++
		}
	}
	if n != want {
		t.Errorf("reminded %d lecturers but the checklist lists %d unsigned — "+
			"the button and the board must chase the same people", n, want)
	}
	if n != 1 {
		t.Errorf("reminded %d, want 1 — the co-teacher submitted nothing to sign", n)
	}
}
