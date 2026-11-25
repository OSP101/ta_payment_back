package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func approvedMergeFixture(t *testing.T) (*fixture, *TeachingService, uuid.UUID, uuid.UUID, ExistingMergeInput) {
	t.Helper()
	f, s, target, source := existingMergeFixture(t)
	in, p := existingMergePlan(t, f, s, target, source)
	f.CourseID = target
	f.SectionID = p.Target.Sections[0].ID
	f.RequestID = f.insertRequest(fixtureOpts{ReimburseScope: "both", RequestStatus: "approved"})
	f.AssignmentID = f.insertAssignment(fixtureOpts{Level: "undergrad", Workload: workloadHours{Attendance: 1, CheckWork: 1}})
	f.exec(`INSERT INTO ta_request_counts(request_id,section_id,undergrad_count) VALUES($1,$2,1)`, f.RequestID, f.SectionID)
	return f, s, target, source, in
}

func mergeApprovalGraph(t *testing.T, f *fixture) string {
	t.Helper()
	var raw []byte
	if err := f.Pool.QueryRow(f.ctx, `SELECT jsonb_build_object('request',to_jsonb(r),
	 'assignments',(SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM ta_request_assignments a WHERE a.request_id=r.id),
	 'workloads',(SELECT jsonb_agg(to_jsonb(w) ORDER BY w.id) FROM ta_workload_forms w JOIN ta_request_assignments a ON a.id=w.assignment_id WHERE a.request_id=r.id),
	 'counts',(SELECT jsonb_agg(to_jsonb(c) ORDER BY c.section_id) FROM ta_request_counts c WHERE c.request_id=r.id)) FROM ta_requests r WHERE r.id=$1`, f.RequestID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestExistingCourseMergeApprovedTargetPreservesApprovalAndWorkload(t *testing.T) {
	f, s, target, source, in := approvedMergeFixture(t)
	// Automatically maintained rows are not evidence of submitted time.
	f.exec(`INSERT INTO course_budget_alerts(teaching_course_id) VALUES($1)`, target)
	pid := uuid.New()
	f.exec(`INSERT INTO submission_periods(id,term_id,year_month,starts_on,due_date,label) VALUES($1,$2,'2569-06','2026-06-01','2026-07-31','test')`, pid, f.TermID)
	f.exec(`INSERT INTO submission_period_status(id,submission_period_id,ta_id,teaching_course_id,status) VALUES($1,$2,$3,$4,'pending')`, uuid.New(), pid, f.TAID, target)
	before := mergeApprovalGraph(t, f)
	p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if err != nil || len(p.Blockers) > 0 {
		t.Fatalf("approved-only preview: %v %v", p.Blockers, err)
	}
	if len(p.ApprovedRequestIDs) != 1 || p.ApprovedRequestIDs[0] != f.RequestID {
		t.Fatal("approved request not captured")
	}
	in.Token = p.Token
	in.Confirmed = true
	if _, err = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); err != nil {
		t.Fatal(err)
	}
	if after := mergeApprovalGraph(t, f); after != before {
		t.Fatal("merge changed approved request, assignments, counts or workloads")
	}
	var regular, special, remaining, snapshots int
	if err = f.Pool.QueryRow(f.ctx, `SELECT num_students_regular,num_students_special FROM teaching_courses WHERE id=$1`, target).Scan(&regular, &special); err != nil {
		t.Fatal(err)
	}
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_courses WHERE id=$1`, source).Scan(&remaining)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM ta_request_calculation_snapshots WHERE request_id=$1 AND event IN ('before_existing_course_merge','existing_course_merged')`, f.RequestID).Scan(&snapshots)
	if regular != 45 || special != 7 || remaining != 0 || snapshots != 2 {
		t.Fatalf("result counts %d/%d source=%d snapshots=%d", regular, special, remaining, snapshots)
	}
	var inputs []byte
	if err = f.Pool.QueryRow(f.ctx, `SELECT inputs FROM ta_request_calculation_snapshots WHERE request_id=$1 AND event='before_existing_course_merge'`, f.RequestID).Scan(&inputs); err != nil {
		t.Fatal(err)
	}
	var evidence map[string]json.RawMessage
	if err = json.Unmarshal(inputs, &evidence); err != nil || len(evidence["sections"]) == 0 {
		t.Fatal("missing pre-merge calculation evidence")
	}
}

func TestExistingCourseMergeApprovedTargetRejectsLaterProgress(t *testing.T) {
	for _, mode := range []string{"draft-time", "submitted-time", "approved-time", "duty-schedule", "lecturer-history", "payment-ledger", "pending-request", "reviewed-month", "future-assignment-reference"} {
		t.Run(mode, func(t *testing.T) {
			f, s, target, source, in := approvedMergeFixture(t)
			switch mode {
			case "draft-time", "submitted-time", "approved-time":
				status := strings.TrimSuffix(mode, "-time")
				f.exec(`INSERT INTO work_logs(assignment_id,work_date,start_time,end_time,hours,activity,status) VALUES($1,CURRENT_DATE,'09:00','10:00',1,'lecture',$2::worklog_status)`, f.AssignmentID, status)
			case "duty-schedule":
				f.exec(`INSERT INTO ta_review_schedules(assignment_id,day_of_week,start_time,end_time) VALUES($1,1,'09:00','10:00')`, f.AssignmentID)
			case "lecturer-history":
				f.exec(`INSERT INTO work_log_lecturer_changes(assignment_id,work_log_id,action,work_date,before,reason,actor_role) VALUES($1,$2,'cut',CURRENT_DATE,'{}','test','lecturer')`, f.AssignmentID, uuid.New())
			case "payment-ledger":
				f.exec(`INSERT INTO export_month_ledger(teaching_course_id,ta_id,year_month,track,earned_baht,paid_baht) VALUES($1,$2,'2026-06','regular',100,100)`, target, f.TAID)
			case "pending-request":
				f.exec(`UPDATE ta_requests SET status='submitted' WHERE id=$1`, f.RequestID)
			case "reviewed-month":
				pid := uuid.New()
				f.exec(`INSERT INTO submission_periods(id,term_id,year_month,starts_on,due_date,label) VALUES($1,$2,'2569-06','2026-06-01','2026-07-31','test')`, pid, f.TermID)
				f.exec(`INSERT INTO submission_period_status(id,submission_period_id,ta_id,teaching_course_id,status,staff_reviewed_by) VALUES($1,$2,$3,$4,'staff_reviewed',$5)`, uuid.New(), pid, f.TAID, target, f.StaffID)
			case "future-assignment-reference":
				f.exec(`CREATE TABLE merge_assignment_probe(id uuid PRIMARY KEY,assignment_id uuid REFERENCES ta_request_assignments(id))`)
				f.exec(`INSERT INTO merge_assignment_probe VALUES($1,$2)`, uuid.New(), f.AssignmentID)
			}
			before := mergeApprovalGraph(t, f)
			p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Blockers) == 0 {
				t.Fatal("later progress was allowed")
			}
			in.Token = p.Token
			in.Confirmed = true
			if _, err = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); err == nil {
				t.Fatal("blocked merge committed")
			}
			var n int
			f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_courses WHERE id=ANY($1::uuid[])`, []uuid.UUID{target, source}).Scan(&n)
			if n != 2 || mergeApprovalGraph(t, f) != before {
				t.Fatal("blocked merge changed data")
			}
		})
	}
}

func TestExistingCourseMergeApprovedTargetRechecksAfterPreview(t *testing.T) {
	for _, mode := range []string{"time-started", "workload-changed"} {
		t.Run(mode, func(t *testing.T) {
			f, s, target, source, in := approvedMergeFixture(t)
			p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
			if err != nil || len(p.Blockers) > 0 {
				t.Fatalf("initial preview: %v %v", p.Blockers, err)
			}
			if mode == "time-started" {
				f.exec(`INSERT INTO work_logs(assignment_id,work_date,start_time,end_time,hours,activity) VALUES($1,CURRENT_DATE,'09:00','10:00',1,'lecture')`, f.AssignmentID)
			} else {
				f.exec(`UPDATE ta_workload_forms SET attendance_hrs=2 WHERE assignment_id=$1`, f.AssignmentID)
			}
			in.Token = p.Token
			in.Confirmed = true
			if _, err = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); err == nil {
				t.Fatal("stale approved-only preview committed")
			}
			var n int
			f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM teaching_courses WHERE id=ANY($1::uuid[])`, []uuid.UUID{target, source}).Scan(&n)
			if n != 2 {
				t.Fatal("stale merge removed a course")
			}
		})
	}
}

func TestExistingCourseMergeApprovedSourceStillBlocked(t *testing.T) {
	f, s, target, source, in := approvedMergeFixture(t)
	f.exec(`INSERT INTO ta_requests(teaching_course_id,lecturer_id,reimburse_scope,status) VALUES($1,$2,'both','approved')`, source, f.LecturerID)
	p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Blockers, " "), "วิชาที่จะนำมารวมมีข้อมูลการใช้งาน") {
		t.Fatalf("source usage was allowed: %v", p.Blockers)
	}
}

func TestExistingCourseMergeApprovedTargetAllowsAnotherPreTimeMerge(t *testing.T) {
	f, s, target, _, in := approvedMergeFixture(t)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			id, err := s.Create(f.ctx, f.StaffID, CreateTeachingCourseInput{TermID: f.TermID, Code: "SC999002", NameTH: "SAME CLASS", Credits: 3, LectureHrs: 3, LecturerIDs: []uuid.UUID{f.LecturerID}, Sections: []CourseSectionInput{{SecNo: "1", Track: "regular", NumStudents: 8}}})
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, ExistingMergeInput{SourceID: id}, false)
			if err != nil {
				t.Fatal(err)
			}
			section := f.SectionID
			in = ExistingMergeInput{SourceID: id, Mappings: []ExistingMergeMapping{{Source: p.Source.Sections[0].ID, Target: &section}}}
		}
		p, err := s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
		if err != nil || len(p.Blockers) > 0 {
			t.Fatalf("attempt%d: %v %v", attempt, p.Blockers, err)
		}
		in.Token = p.Token
		in.Confirmed = true
		if _, err = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, true); err != nil {
			t.Fatal(err)
		}
	}
	var snapshots int
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM ta_request_calculation_snapshots WHERE request_id=$1`, f.RequestID).Scan(&snapshots)
	if snapshots != 4 {
		t.Fatalf("snapshot count=%d", snapshots)
	}
}

func TestExistingCourseMergeApprovedTargetSerializesNewTime(t *testing.T) {
	f, s, target, _, in := approvedMergeFixture(t)
	tx, err := f.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	p, err := prepareExistingMerge(f.ctx, tx, target, in)
	if err != nil || len(p.Blockers) > 0 {
		t.Fatalf("initial inspection: %v %v", p.Blockers, err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.Pool.Exec(ctx, `/* merge-approved-time-race */ INSERT INTO work_logs(assignment_id,work_date,start_time,end_time,hours,activity) VALUES($1,CURRENT_DATE,'09:00','10:00',1,'lecture')`, f.AssignmentID)
		done <- err
	}()
	locked := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if err = f.Pool.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND query LIKE '/* merge-approved-time-race */%' AND wait_event_type='Lock')`).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		if locked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("time write did not wait for merge locks: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !locked {
		t.Fatal("new time did not reach the merge's row lock")
	}
	if err = tx.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	p, err = s.ExistingCourseMerge(f.ctx, f.StaffID, target, in, false)
	if err != nil || !strings.Contains(strings.Join(p.Blockers, " "), "บันทึกเวลาปฏิบัติงาน") {
		t.Fatalf("new time not detected after lock release: %v %v", p.Blockers, err)
	}
}
