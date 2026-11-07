package service

import (
	"context"
	"github.com/google/uuid"
	"sync"
	"testing"
	"time"
)

func TestConcurrentCoLecturerSubmissionsReserveMissingTimetableTAOnce(t *testing.T) {
	f := newRequestFixture(t, fixtureOpts{})
	other := f.insertUser("lecturer", "co-lecturer")
	f.exec(`INSERT INTO teaching_lecturers(teaching_course_id,lecturer_id,is_primary) VALUES($1,$2,false)`, f.CourseID, other)
	ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, lect := range []uuid.UUID{f.LecturerID, other} {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			<-start
			_, err := f.Req.Create(ctx, id, f.createInput())
			results <- err
		}(lect)
	}
	close(start)
	wg.Wait()
	close(results)
	ok, conflicts := 0, 0
	for err := range results {
		if err == nil {
			ok++
		} else if userErrStatus(err) == 409 {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.Pool.QueryRow(ctx, `SELECT count(*) FROM ta_requests WHERE teaching_course_id=$1 AND status IN ('submitted','approved')`, f.CourseID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if ok != 1 || conflicts != 1 || count != 1 {
		t.Fatalf("success=%d conflicts=%d active=%d", ok, conflicts, count)
	}
}

func TestConcurrentCoLecturerPartialOverlapIsAtomic(t *testing.T) {
	for _, mode := range []string{"partial", "disjoint", "reversed"} {
		t.Run(mode, func(t *testing.T) {
			f := newRequestFixture(t, fixtureOpts{})
			other := f.insertUser("lecturer", "colleague")
			f.exec(`INSERT INTO teaching_lecturers(teaching_course_id,lecturer_id,is_primary) VALUES($1,$2,false)`, f.CourseID, other)
			a, b, c, d := f.TAID, f.insertUser("ta", "B"), f.insertUser("ta", "C"), f.insertUser("ta", "D")
			left, right := []uuid.UUID{a, b}, []uuid.UUID{b, c}
			if mode == "disjoint" {
				right = []uuid.UUID{c, d}
			}
			if mode == "reversed" {
				right = []uuid.UUID{b, a}
			}
			input := func(ids []uuid.UUID) CreateTARequestInput {
				in := f.createInput()
				template := in.Assignments[0]
				in.Assignments = nil
				for _, id := range ids {
					a := template
					a.TAID = id
					in.Assignments = append(in.Assignments, a)
				}
				return in
			}
			ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
			defer cancel()
			start := make(chan struct{})
			results := make(chan error, 2)
			go func() { <-start; _, err := f.Req.Create(ctx, f.LecturerID, input(left)); results <- err }()
			go func() { <-start; _, err := f.Req.Create(ctx, other, input(right)); results <- err }()
			close(start)
			success := 0
			for i := 0; i < 2; i++ {
				err := <-results
				if err == nil {
					success++
				} else if userErrStatus(err) != 409 {
					t.Fatal(err)
				}
			}
			want := 1
			if mode == "disjoint" {
				want = 2
			}
			if success != want {
				t.Fatalf("success=%d want=%d", success, want)
			}
			var count, people int
			if err := f.Pool.QueryRow(ctx, `SELECT count(*),count(DISTINCT a.ta_id) FROM ta_request_assignments a JOIN ta_requests r ON r.id=a.request_id WHERE r.teaching_course_id=$1 AND r.status IN ('submitted','approved') AND a.state<>'dropped'`, f.CourseID).Scan(&count, &people); err != nil {
				t.Fatal(err)
			}
			if count != want*2 || people != count {
				t.Fatalf("assignments=%d distinct=%d; partial write or duplicate", count, people)
			}
		})
	}
}
