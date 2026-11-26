package handler

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"os"
	"testing"
)

func (f *mergeHTTPFixture) realTerm2Courses(t *testing.T, year int) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	term := uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO academic_terms(id,academic_year,semester) VALUES($1,$2,2)`, term, year); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("MERGE_REAL_REGISTRAR_FILE")
	if path == "" {
		path = "../../../docs/รายวิชาที่เปิดสอนภาค 2-2569.xls"
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.Teaching.PreviewREGReview(ctx, f.actor, term, "term2.xls", body, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := p.Review
	for i := range r.Courses {
		c := &r.Courses[i]
		if c.Code != "CP321004" && c.Code != "SC361004" {
			c.Skip = true
			c.SkipReason = "Reproduce reported course pair"
		} else {
			c.LecturerIDs = []uuid.UUID{f.actor}
		}
	}
	p, err = f.svc.Teaching.PreviewREGReview(ctx, f.actor, term, "term2.xls", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	for _, issue := range r.Issues {
		if issue.Severity == "error" {
			t.Fatalf("review: %+v", issue)
		}
		r.Acknowledged = append(r.Acknowledged, issue.ID)
	}
	p, err = f.svc.Teaching.PreviewREGReview(ctx, f.actor, term, "term2.xls", body, r)
	if err != nil {
		t.Fatal(err)
	}
	r = p.Review
	r.Confirmed = true
	if _, err = f.svc.Teaching.CommitREGReview(ctx, f.actor, term, "term2.xls", body, r); err != nil {
		t.Fatal(err)
	}
	var target, source uuid.UUID
	for code, id := range map[string]*uuid.UUID{"CP321004": &target, "SC361004": &source} {
		if err = f.pool.QueryRow(ctx, `SELECT id FROM teaching_courses WHERE term_id=$1 AND code=$2`, term, code).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	return term, target, source
}

func (f *mergeHTTPFixture) seedRealTerm2References(t *testing.T, term, target, source uuid.UUID, year, offset int) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO ta_request_drafts(user_id,teaching_course_id,payload) VALUES($1,$2,'{"v":1,"scope":"both","assignments":[]}'::jsonb)`, f.actor, target)
	data, err := os.ReadFile("../service/testdata/merge-term2-tdbm.json")
	if err != nil {
		t.Fatal(err)
	}
	var refs []map[string]any
	if err = json.Unmarshal(data, &refs); err != nil {
		t.Fatal(err)
	}
	for i, row := range refs {
		tc := target
		if row["course_code"] == "SC361004" {
			tc = source
		}
		var sec uuid.UUID
		if err = f.pool.QueryRow(ctx, `SELECT id FROM sections WHERE teaching_course_id=$1 AND sec_no::int=$2::int`, tc, row["section_no"]).Scan(&sec); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(row)
		exec(`INSERT INTO tdbm_extra_teachings(extra_class_id,academic_year,semester,course_code,teaching_course_id,section_id,section_label,semester_type,opt_status,class_date,start_time,end_time,duration_minutes) SELECT $1,$2,2,r.course_code,$3,$4,r.section_label,r.semester_type,r.opt_status,r.class_date,r.start_time,r.end_time,r.duration_minutes FROM jsonb_to_record($5::jsonb) AS r(course_code text,section_label text,semester_type text,opt_status text,class_date date,start_time time,end_time time,duration_minutes int)`, 990000+offset*100+i, year, tc, sec, raw)
	}
	// Four approved requests, each with the two regular main-course sections.
	// The caller already seeded the first request/assignment.
	var firstRequest, secondSection uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM ta_requests WHERE teaching_course_id=$1 LIMIT 1`, target).Scan(&firstRequest); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id FROM sections WHERE teaching_course_id=$1 AND sec_no::int=2`, target).Scan(&secondSection); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		rid, ta := firstRequest, uuid.Nil
		if i == 0 {
			if err := f.pool.QueryRow(ctx, `SELECT ta_id FROM ta_request_assignments WHERE request_id=$1 LIMIT 1`, rid).Scan(&ta); err != nil {
				t.Fatal(err)
			}
		} else {
			rid, ta = uuid.New(), uuid.New()
			exec(`INSERT INTO users(id,email,first_name,last_name,is_active) VALUES($1,$2,'TA','Tester',true)`, ta, ta.String()+"@example.test")
			exec(`INSERT INTO user_roles(user_id,role) VALUES($1,'ta')`, ta)
			exec(`INSERT INTO ta_requests(id,teaching_course_id,lecturer_id,reimburse_scope,status,submitted_at,decided_at,decided_by) VALUES($1,$2,$3,'both','approved',NOW(),NOW(),$3)`, rid, target, f.actor)
		}
		rows, err := f.pool.Query(ctx, `SELECT id FROM sections WHERE teaching_course_id=$1 AND track='regular' ORDER BY sec_no`, target)
		if err != nil {
			t.Fatal(err)
		}
		var secs []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err = rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			secs = append(secs, id)
		}
		rows.Close()
		for _, sec := range secs {
			if i == 0 && sec != secondSection {
				continue
			}
			aid := uuid.New()
			exec(`INSERT INTO ta_request_counts(request_id,section_id,undergrad_count) VALUES($1,$2,1)`, rid, sec)
			exec(`INSERT INTO ta_request_assignments(id,request_id,section_id,ta_id,level) VALUES($1,$2,$3,$4,'undergrad')`, aid, rid, sec, ta)
			exec(`INSERT INTO ta_workload_forms(assignment_id,attendance_hrs,lab_hrs,check_work_hrs) VALUES($1,.5,1,1)`, aid)
		}
	}
	exec(`UPDATE ta_workload_forms w SET attendance_hrs=.5,lab_hrs=1,check_work_hrs=1 FROM ta_request_assignments a JOIN ta_requests r ON r.id=a.request_id WHERE w.assignment_id=a.id AND r.teaching_course_id=$1`, target)
}
