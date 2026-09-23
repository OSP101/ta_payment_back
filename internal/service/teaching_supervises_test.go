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

	check := func(lect, term uuid.UUID, want bool, label string) {
		t.Helper()
		got, err := svc.LecturerSupervisesTA(ctx, lect, ta, term)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got != want {
			t.Errorf("%s: got %v, want %v", label, got, want)
		}
	}
	check(owner, termID, true, "requesting lecturer")
	check(coLect, termID, true, "co-lecturer on the course")
	check(outside, termID, false, "unrelated lecturer")

	// Scoped to the term asked about: one assignment is not a pass to the TA's
	// timetable in every other term.
	var otherTerm uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO academic_terms (academic_year, semester) VALUES (9968, 2) RETURNING id`,
	).Scan(&otherTerm); err != nil {
		t.Fatal(err)
	}
	check(owner, otherTerm, false, "same lecturer, a term with no assignment")

	// A dropped assignment no longer supervises.
	if _, err := pool.Exec(ctx, `UPDATE ta_request_assignments SET state = 'dropped' WHERE request_id = $1`, reqID); err != nil {
		t.Fatal(err)
	}
	check(owner, termID, false, "assignment dropped")
	if _, err := pool.Exec(ctx, `UPDATE ta_request_assignments SET state = DEFAULT WHERE request_id = $1`, reqID); err != nil {
		t.Fatal(err)
	}

	// Assignment rows are written at SUBMIT and never deleted, so a request that
	// was rejected or cancelled must not keep granting access.
	for _, st := range []string{"rejected", "cancelled", "submitted"} {
		if _, err := pool.Exec(ctx, `UPDATE ta_requests SET status = $2::ta_request_status WHERE id = $1`, reqID, st); err != nil {
			t.Fatal(err)
		}
		check(owner, termID, false, "request "+st)
	}
}
