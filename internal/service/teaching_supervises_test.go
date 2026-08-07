package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/testutil"
)

// The timetable form is a TA's personal weekly whereabouts. Before this check
// any lecturer account could iterate user ids through ?user_id= and pull every
// TA's schedule; supervision — an assignment in one of the lecturer's own
// courses — is what opens it.
func TestLecturerSupervisesTA(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &TeachingService{pool: pool}

	mkUser := func() uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, first_name, last_name) VALUES ($1, $2, 'ท', 'ท')`,
			id, id.String()+"@test.local"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	owner := mkUser()   // the lecturer who requested the TA
	coLect := mkUser()  // co-lecturer on the same course
	outside := mkUser() // a lecturer with no tie to the course
	ta := mkUser()

	var termID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO academic_terms (academic_year, semester) VALUES (9968, 1) RETURNING id`,
	).Scan(&termID); err != nil {
		t.Fatal(err)
	}
	var tcID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO teaching_courses (term_id, code, name_th, num_students)
		 VALUES ($1, 'SEC999', 'วิชาทดสอบสิทธิ์', 10) RETURNING id`, termID,
	).Scan(&tcID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO teaching_lecturers (teaching_course_id, lecturer_id, is_primary)
		 VALUES ($1, $2, TRUE), ($1, $3, FALSE)`, tcID, owner, coLect); err != nil {
		t.Fatal(err)
	}
	var secID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO sections (teaching_course_id, sec_no, track, num_students)
		 VALUES ($1, 1, 'regular', 10) RETURNING id`, tcID,
	).Scan(&secID); err != nil {
		t.Fatal(err)
	}
	var reqID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO ta_requests (teaching_course_id, lecturer_id, reimburse_scope, status)
		 VALUES ($1, $2, 'both', 'approved') RETURNING id`, tcID, owner,
	).Scan(&reqID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO ta_request_assignments (request_id, section_id, ta_id, level)
		 VALUES ($1, $2, $3, 'undergrad')`, reqID, secID, ta); err != nil {
		t.Fatal(err)
	}

	check := func(lect uuid.UUID, want bool, label string) {
		t.Helper()
		got, err := svc.LecturerSupervisesTA(ctx, lect, ta)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got != want {
			t.Errorf("%s: got %v, want %v", label, got, want)
		}
	}
	check(owner, true, "requesting lecturer")
	check(coLect, true, "co-lecturer on the course")
	check(outside, false, "unrelated lecturer")
}
