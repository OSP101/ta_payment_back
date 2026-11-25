package service

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfmodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func TestEditRequest_ReplacesAtomicallyAndPrefillsWorkload(t *testing.T) {
	f := newRequestFixture(t, fixtureOpts{})
	f.addClassFor(f.TAID)
	old, err := f.Req.Create(f.ctx, f.LecturerID, f.createInput())
	if err != nil {
		t.Fatal(err)
	}
	in, err := f.Req.EditInput(f.ctx, f.LecturerID, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Assignments) != 1 || len(in.Assignments[0].SectionWorkloads) != 1 {
		t.Fatalf("incomplete edit input: %+v", in)
	}
	in.Assignments[0].SectionWorkloads[0].Workload.CheckWorkHrs = 1
	edited, err := f.Req.Edit(f.ctx, f.LecturerID, old.ID, *in)
	if err != nil {
		t.Fatal(err)
	}
	if f.status(old.ID) != "cancelled" || f.status(edited.ID) != "approved" {
		t.Fatal("replacement not committed")
	}
	var hours float64
	f.Pool.QueryRow(f.ctx, `SELECT w.check_work_hrs::float8 FROM ta_workload_forms w JOIN ta_request_assignments a ON a.id=w.assignment_id WHERE a.request_id=$1`, edited.ID).Scan(&hours)
	if hours != 1 {
		t.Fatalf("edited workload = %v", hours)
	}
	// Failing automatic validation must roll back the cancellation too.
	in.Assignments[0].SectionWorkloads[0].Workload = WorkloadInput{}
	if _, err := f.Req.Edit(f.ctx, f.LecturerID, edited.ID, *in); err == nil {
		t.Fatal("empty workload accepted")
	}
	if f.status(edited.ID) != "approved" {
		t.Fatal("failed edit cancelled the original")
	}
	if _, err := f.Req.EditInput(f.ctx, f.StaffID, edited.ID); err == nil {
		t.Fatal("other user's request exposed")
	}
}

func TestEditRequest_IssuedPersonLockedOtherPersonEditable(t *testing.T) {
	f := newRequestFixture(t, fixtureOpts{})
	f.addClassFor(f.TAID)
	first, err := f.Req.Create(f.ctx, f.LecturerID, f.createInput())
	if err != nil {
		t.Fatal(err)
	}
	order := uuid.New()
	f.exec(`INSERT INTO appointment_orders(id,term_id,round_no,order_no,order_date,effective_date,ta_count) VALUES($1,$2,1,'1','today','today',1)`, order, f.TermID)
	f.exec(`INSERT INTO appointment_order_items(id,appointment_order_id,teaching_course_id,ta_id) VALUES(gen_random_uuid(),$1,$2,$3)`, order, f.CourseID, f.TAID)
	in, _ := f.Req.EditInput(f.ctx, f.LecturerID, first.ID)
	if _, err := f.Req.Edit(f.ctx, f.LecturerID, first.ID, *in); err == nil {
		t.Fatal("issued TA edited")
	}
	if err := f.Req.Cancel(f.ctx, f.LecturerID, first.ID); err == nil {
		t.Fatal("issued TA cancelled")
	}
	if err := f.Req.UpdateAssignmentWorkload(f.ctx, f.StaffID, f.assignmentID(first.ID), WorkloadInput{CheckWorkHrs: 1}); err == nil {
		t.Fatal("issued workload edited")
	}
	if _, err := f.Req.AddSections(f.ctx, f.LecturerID, first.ID, AddSectionsInput{TAID: f.TAID, Sections: []SectionWorkload{{SectionID: f.SectionID}}}); err == nil || !strings.Contains(err.Error(), "ใบแต่งตั้ง") {
		t.Fatalf("issued TA sections were not locked: %v", err)
	}
	f.saveTimetable(1, "09:00", "12:00")
	f.saveTimetable(1, "13:00", "16:00")
	if f.status(first.ID) != "approved" {
		t.Fatal("timetable recheck removed an issued person")
	}
	detail, err := f.Req.Detail(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.CanCancel {
		t.Fatal("issued row offers cancel")
	}
	second := f.insertUser("ta", "second")
	// A second person on the same course remains editable.
	f.addClassFor(second)
	otherIn := f.createInput()
	otherIn.Assignments[0].TAID = second
	other, err := f.Req.Create(f.ctx, f.LecturerID, otherIn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Req.Edit(f.ctx, f.LecturerID, other.ID, otherIn); err != nil {
		t.Fatal(err)
	}
}

func TestGradLumpAllocations_ShortBudgetSharedEqually(t *testing.T) {
	f := newFixture(t, fixtureOpts{Level: "master", Track: "special"})
	// Two distinct people, including one holding more than one section, must
	// receive one equal share each, rather than one share per assignment.
	other := f.insertUser("ta", "second")
	f.exec(`INSERT INTO ta_request_assignments(id,request_id,section_id,ta_id,level) VALUES(gen_random_uuid(),$1,$2,$3,'phd')`, f.RequestID, f.SectionID, other)
	f.exec(`UPDATE teaching_courses SET num_students=1,num_students_special=1,num_students_regular=0 WHERE id=$1`, f.CourseID)
	budget, err := (&BudgetService{pool: f.Pool}).compute(f.ctx, f.CourseID, false)
	if err != nil {
		t.Fatal(err)
	}
	exp := &ExportService{pool: f.Pool}
	shares, err := exp.gradLumpAllocations(f.ctx, f.CourseID, 4000)
	if err != nil {
		t.Fatal(err)
	}
	want := math.Min(4000, math.Floor(budget.TermPaySpecial*100/2)/100)
	if shares[f.TAID] != want || shares[other] != want {
		t.Fatalf("shares %+v, want %v each", shares, want)
	}
	settlement, err := exp.ForecastCourse(f.ctx, f.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(settlement.Special.Committed-2*want) > 0.01 {
		t.Fatalf("settlement committed %v, want %v", settlement.Special.Committed, 2*want)
	}
	f.exec(`UPDATE teaching_courses SET num_students=0,num_students_special=0,num_students_regular=0 WHERE id=$1`, f.CourseID)
	empty, err := exp.gradLumpAllocations(f.ctx, f.CourseID, 4000)
	if err != nil {
		t.Fatal(err)
	}
	for _, amount := range empty {
		if amount != 0 {
			t.Fatalf("zero budget allocated %v", amount)
		}
	}

}

func TestBuildAllApprovedBundle_OneMergedPDFPerPerson(t *testing.T) {
	page := realPDF(t)
	svc, ids := bundleFixture(t, [][]byte{page, page, page}, []string{"creditor_form", "national_id", "bank_book"})
	ctx := context.Background()
	other := uuid.New()
	if _, err := svc.pool.Exec(ctx, `INSERT INTO users(id,email,first_name,last_name,student_id,is_active) VALUES($1,$2,'คนที่','สอง','653020999-9',TRUE)`, other, other.String()+"@test.example"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"creditor_form", "national_id", "bank_book"} {
		id := uuid.New()
		key, _, err := svc.store.Save("ta_docs", kind+".pdf", bytes.NewReader(page))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.pool.Exec(ctx, `INSERT INTO ta_documents(id,user_id,kind,filename,mime,size_bytes,storage_key,status) VALUES($1,$2,$3,$4,'application/pdf',$5,$6,'approved')`, id, other, kind, kind+".pdf", len(page), key); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Actor may be any real user; recording has a FK to users.
	body, name, err := svc.BuildAllApprovedBundle(ctx, other, ids, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, ".zip") {
		t.Fatalf("name %s", name)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("zip has %d entries", len(zr.File))
	}
	conf := pdfmodel.NewDefaultConfiguration()
	conf.ValidationMode = pdfmodel.ValidationRelaxed
	sourcePages, err := pdfapi.PageCount(bytes.NewReader(page), conf)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, file := range zr.File {
		names[file.Name] = true
		rc, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if dir := os.Getenv("TA_FIXES_QA_DIR"); dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, file.Name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		pages, err := pdfapi.PageCount(bytes.NewReader(data), conf)
		if err != nil {
			t.Fatal(err)
		}
		if pages != sourcePages*3 {
			t.Fatalf("%s pages=%d", file.Name, pages)
		}
	}
	for _, name := range []string{"653020123-4_ทดสอบ_ผู้ใช้.pdf", "653020999-9_คนที่_สอง.pdf"} {
		if !names[name] {
			t.Errorf("missing %s", name)
		}
	}
}

func TestEditRequest_GraduateDailyHoursRemainRequestable(t *testing.T) {
	f := newRequestFixture(t, fixtureOpts{Level: "master"})
	f.exec(`UPDATE users SET study_level='master' WHERE id=$1`, f.TAID)
	f.addClassFor(f.TAID)
	section := f.addEightHourSection("02", 5)
	in := f.inputFor([]SectionWorkload{{SectionID: section, Workload: WorkloadInput{HelpTeachHrs: 6}}}, "master")
	old, err := f.Req.Create(f.ctx, f.LecturerID, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Assignments[0].SectionWorkloads[0].Workload.HelpTeachHrs = 8
	changed, err := f.Req.Edit(f.ctx, f.LecturerID, old.ID, in)
	if err != nil {
		t.Fatalf("graduate request >6 daily hours must remain allowed: %v", err)
	}
	if changed.Status != "approved" {
		t.Fatalf("status %s", changed.Status)
	}
	in.Assignments[0].SectionWorkloads[0].Workload.HelpTeachHrs = 13
	if _, err := f.Req.Edit(f.ctx, f.LecturerID, changed.ID, in); err == nil {
		t.Fatal("weekly ceiling must still apply")
	}
	if f.status(changed.ID) != "approved" {
		t.Fatal("failed edit damaged original request")
	}
}

func TestBuildAllApprovedBundle_RejectsIncompletePerson(t *testing.T) {
	page := realPDF(t)
	svc, ids := bundleFixture(t, [][]byte{page, page}, []string{"national_id", "bank_book"})
	if _, _, err := svc.BuildAllApprovedBundle(context.Background(), uuid.Nil, ids, 1); err == nil {
		t.Fatal("partial person exported")
	}
}
