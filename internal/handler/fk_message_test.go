package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/testutil"
)

// Deleting a section that still has TAs used to answer only "มีข้อมูลอ้างอิงอยู่".
// Against a real Postgres FK error (so the TableName assumption is checked
// too) the message must now name what is in the way.
func TestFKViolationNamesTheReferrer(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	term, lec, ta, tc, sec, req := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO academic_terms (id, academic_year, semester, is_active) VALUES ($1, 2569, 1, TRUE)`, term)
	exec(`INSERT INTO users (id,email,first_name,last_name,is_active) VALUES ($1,$2,'อ','ท',TRUE)`, lec, "l-"+lec.String()+"@example.test")
	exec(`INSERT INTO users (id,email,first_name,last_name,is_active) VALUES ($1,$2,'ท','ท',TRUE)`, ta, "t-"+ta.String()+"@example.test")
	exec(`INSERT INTO teaching_courses (id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs,num_students)
	      VALUES ($1,$2,'CP001','ทดสอบ','undergrad',3,2,2,5,40)`, tc, term)
	exec(`INSERT INTO sections (id,teaching_course_id,sec_no,track) VALUES ($1,$2,'01','regular')`, sec, tc)
	exec(`INSERT INTO ta_requests (id,teaching_course_id,lecturer_id,reimburse_scope,status,submitted_at)
	      VALUES ($1,$2,$3,'both','approved',NOW())`, req, tc, lec)
	exec(`INSERT INTO ta_request_assignments (request_id,section_id,ta_id,level) VALUES ($1,$2,$3,'undergrad')`, req, sec, ta)

	_, err := pool.Exec(ctx, `DELETE FROM sections WHERE id = $1`, sec)
	if err == nil {
		t.Fatal("expected a foreign-key violation")
	}
	status, msg := errorResponse(err)
	if status != 409 {
		t.Errorf("status = %d, want 409", status)
	}
	if !strings.Contains(msg, "TA") || strings.Contains(msg, "ta_request") {
		t.Errorf("message = %q, want it to name the TAs (in Thai, no table names)", msg)
	}
}
