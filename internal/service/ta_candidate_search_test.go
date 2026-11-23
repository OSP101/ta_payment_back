package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCandidates_SearchAndRestore(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.exec(`UPDATE users SET first_name='สมชาย', last_name='ใจดี', email='find_me@example.test', student_id='67010042' WHERE id=$1`, rf.TAID)
	rf.exec(`INSERT INTO users (id,email,first_name,last_name,password_hash) VALUES (gen_random_uuid(),'findXme@example.test','สมชาย','อื่น','unused')`)
	for _, query := range []string{" สมชาย ใจดี ", "FIND_ME@EXAMPLE.TEST", "67010042"} {
		got, err := rf.Req.Candidates(rf.ctx, rf.CourseID, CandidateFilter{Search: query, Limit: 31})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != rf.TAID {
			t.Fatalf("query %q: %+v", query, got)
		}
	}
	// A literal SQL wildcard must not match every account.
	got, err := rf.Req.Candidates(rf.ctx, rf.CourseID, CandidateFilter{Search: "%", Limit: 31})
	if err != nil || len(got) != 0 {
		t.Fatalf("literal wildcard: %v %v", got, err)
	}
	got, err = rf.Req.Candidates(rf.ctx, rf.CourseID, CandidateFilter{IDs: []uuid.UUID{rf.TAID}, Limit: 100})
	if err != nil || len(got) != 1 || got[0].StudentID != "67010042" {
		t.Fatalf("restore: %v %v", got, err)
	}
	if _, err = rf.Req.Create(rf.ctx, rf.LecturerID, rf.createInput()); err != nil {
		t.Fatal(err)
	}
	got, err = rf.Req.Candidates(rf.ctx, rf.CourseID, CandidateFilter{IDs: []uuid.UUID{rf.TAID}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].AlreadyInCourse || got[0].ExistingRequestID == nil || len(got[0].HeldSectionIDs) != 1 {
		t.Fatalf("held sections lost: %+v", got)
	}

	rf.secondCourseAssignment(fixtureOpts{})
	rf.setCourseCap(1)
	got, err = rf.Req.Candidates(rf.ctx, rf.CourseID, CandidateFilter{Search: "67010042", Limit: 31})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].AtQuota || got[0].CourseCap != 1 || got[0].ApprovedCourseCount != 1 {
		t.Fatalf("quota must stay searchable and use settings: %+v", got)
	}
}

func TestCandidates_PagingAtScale(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	rf.exec(`WITH inserted AS (
 INSERT INTO users(id,email,first_name,last_name,password_hash,student_id)
 SELECT gen_random_uuid(), 'candidate-'||n||'@example.test','Candidate',lpad(n::text,5,'0'),'unused', 'S'||lpad(n::text,5,'0') FROM generate_series(1,3000) n RETURNING id)
 INSERT INTO user_roles(user_id,role) SELECT id,'ta' FROM inserted`)
	started := time.Now()
	all, err := rf.Req.Candidates(rf.ctx, rf.CourseID)
	if err != nil {
		t.Fatal(err)
	}
	allTime := time.Since(started)
	seen := map[uuid.UUID]bool{}
	started = time.Now()
	for offset := 0; offset < 90; offset += 30 {
		got, err := rf.Req.Candidates(rf.ctx, rf.CourseID, CandidateFilter{Search: "Candidate", Limit: 31, Offset: offset})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 31 {
			t.Fatalf("page %d: %d rows", offset, len(got))
		}
		for _, c := range got[:30] {
			if seen[c.ID] {
				t.Fatal("duplicate page result")
			}
			seen[c.ID] = true
		}
	}
	pageTime := time.Since(started) / 3
	for _, query := range []string{"S03000", "candidate-3000@example.test"} {
		got, err := rf.Req.Candidates(rf.ctx, rf.CourseID, CandidateFilter{Search: query, Limit: 31})
		if err != nil || len(got) != 1 {
			t.Fatalf("search %s: %d %v", query, len(got), err)
		}
	}
	t.Log(fmt.Sprintf("%d candidates: full=%s; average bounded page=%s", len(all), allTime, pageTime))
}
