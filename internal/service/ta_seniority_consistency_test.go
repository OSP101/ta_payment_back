package service

import (
	"testing"

	"github.com/google/uuid"
	"ta-payment-back/internal/audit"
)

func TestTASeniority_AllConsumersUseLegacyOverrideAndFirstAppointment(t *testing.T) {
	t.Chdir(repoRoot(t))
	f := newTCFixture(t)
	course, sec, _ := f.insertCourse(tcCourseOpts{Code: "CP990001", Curriculum: "CY", LectureHrs: 10})
	legacy := f.newTA("ระบบเดิม", "undergrad")
	first := f.newTA("ครั้งแรก", "undergrad")
	earlier := f.newTA("เคยทำงาน", "undergrad")
	dropped := f.newTA("คำขอเก่าถูกถอด", "undergrad")
	// A legacy override must beat a misleading stamp in the current term.
	f.exec(`UPDATE users SET ta_seniority_override='returning',ta_first_term_id=$2 WHERE id=$1`, legacy, f.termID)
	current := f.termID
	f.termID = uuid.New()
	f.exec(`INSERT INTO academic_terms(id,academic_year,semester) VALUES($1,2568,2)`, f.termID)
	oldCourse, oldSec, _ := f.insertCourse(tcCourseOpts{Code: "CP990000", Curriculum: "CY", LectureHrs: 10})
	f.assignTA(earlier, oldCourse, oldSec, "undergrad", nil)
	droppedAssignment := f.assignTA(dropped, oldCourse, oldSec, "undergrad", nil)
	f.exec(`UPDATE ta_request_assignments SET state='dropped' WHERE id=$1`, droppedAssignment)
	f.termID = current
	for _, id := range []uuid.UUID{legacy, first, earlier, dropped} {
		f.assignTA(id, course, sec, "undergrad", nil)
	}
	users := &UserService{pool: f.pool, aud: audit.New(f.pool)}
	want := map[uuid.UUID]bool{legacy: true, first: false, earlier: true, dropped: false}
	work := &WorkLogService{pool: f.pool}
	assignments, err := work.StaffListAssignments(f.ctx, course)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 4 {
		t.Fatalf("got %d assignments", len(assignments))
	}
	for _, a := range assignments {
		if a.IsReturning != want[a.TAID] {
			t.Errorf("worklog badge disagrees for %s", a.TAID)
		}
	}
	cands, err := f.docs.SupplierCandidates(f.ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	candidates := map[uuid.UUID]bool{}
	for _, c := range cands {
		candidates[c.UserID] = true
	}
	for id, returning := range want {
		classification, err := users.TASeniority(f.ctx, id, current)
		if err != nil {
			t.Fatal(err)
		}
		if (classification == "returning") != returning {
			t.Errorf("resolver disagrees for %s: %s", id, classification)
		}
		if f.svc.isReturningTA(f.ctx, id, current) != returning {
			t.Errorf("export preview disagrees for %s", id)
		}
		if candidates[id] == returning {
			t.Errorf("ERP candidates disagree for %s", id)
		}
		data, err := f.svc.BuildTimetableWorkbook(f.ctx, id, current)
		if err != nil {
			t.Fatal(err)
		}
		wb := openWorkbookBytes(t, data)
		label, err := wb.GetCellValue("ตารางสอน", "AA1")
		wb.Close()
		if err != nil {
			t.Fatal(err)
		}
		if (label == "TA เดิม") != returning {
			t.Errorf("timetable disagrees for %s: %q", id, label)
		}
	}
}
