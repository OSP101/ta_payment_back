package service

import (
	"strings"
	"testing"
	"time"
)

// The lecturer hears about a submission once (05/10/2026): when every TA on
// it is decided, or after 24 hours as a summary.

type lecturerNotice struct{ title, body string }

func (rf *requestFixture) lecturerVerdictNotices() []lecturerNotice {
	rf.t.Helper()
	rows, err := rf.Pool.Query(rf.ctx, `
		SELECT title, body FROM notifications
		WHERE user_id = $1 AND channel = 'in_app'
		  AND (title LIKE 'ผลการพิจารณาคำขอผู้ช่วยสอน%' OR title LIKE 'คำขอผู้ช่วยสอน%')
		ORDER BY created_at`, rf.LecturerID)
	if err != nil {
		rf.t.Fatal(err)
	}
	defer rows.Close()
	var out []lecturerNotice
	for rows.Next() {
		var n lecturerNotice
		if err := rows.Scan(&n.title, &n.body); err != nil {
			rf.t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		rf.t.Fatal(err)
	}
	return out
}

func TestLecturerDigest_EveryoneDecidedAtOnce_OneNotice(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.insertUser("ta", "ta2")
	rf.addClassFor(rf.TAID)
	rf.addClassFor(second)
	// The second TA fails (no hours): one approved, one rejected.
	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.twoTAInput(second, WorkloadInput{})); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := rf.lecturerVerdictNotices()
	if len(got) != 1 {
		t.Fatalf("lecturer got %d verdict notices, want 1: %+v", len(got), got)
	}
	b := got[0].body
	if !strings.Contains(b, "ได้รับการอนุมัติ 1 คน") || !strings.Contains(b, "ไม่ผ่านการอนุมัติ 1 คน") {
		t.Errorf("body should list who passed and who did not:\n%s", b)
	}
	if strings.Contains(b, "อยู่ระหว่างพิจารณา") {
		t.Errorf("nobody is waiting, yet the body says so:\n%s", b)
	}
}

func TestLecturerDigest_WaitsForTheLastTAWithin24Hours(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.insertUser("ta", "ta2")
	rf.addClassFor(rf.TAID) // second has no timetable yet
	res, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.twoTAInput(second, WorkloadInput{AttendanceHrs: 2, LabHrs: 2}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := rf.lecturerVerdictNotices(); len(got) != 0 {
		t.Fatalf("lecturer told before everyone was decided: %+v", got)
	}
	// An hourly sweep inside the 24 hours sends nothing.
	if n, err := rf.Req.SweepLecturerDigests(rf.ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("sweep inside 24h = %d, %v; want 0", n, err)
	}

	rf.addClassFor(second)
	if err := rf.Req.ReevaluateForTA(rf.ctx, second, rf.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}
	if st := rf.status(resultFor(t, res, second).ID); st != "approved" {
		t.Fatalf("second TA = %q, want approved", st)
	}
	got := rf.lecturerVerdictNotices()
	if len(got) != 1 {
		t.Fatalf("lecturer got %d verdict notices, want 1 once both were decided: %+v", len(got), got)
	}
	if !strings.Contains(got[0].body, "ได้รับการอนุมัติ 2 คน") {
		t.Errorf("body should approve both:\n%s", got[0].body)
	}
}

func TestLecturerDigest_SummaryAfter24HoursThenFollowUp(t *testing.T) {
	rf := newRequestFixture(t, fixtureOpts{})
	second := rf.insertUser("ta", "ta2")
	rf.addClassFor(rf.TAID)
	if _, err := rf.Req.Create(rf.ctx, rf.LecturerID, rf.twoTAInput(second, WorkloadInput{AttendanceHrs: 2, LabHrs: 2})); err != nil {
		t.Fatalf("create: %v", err)
	}

	if n, err := rf.Req.SweepLecturerDigests(rf.ctx, time.Now().Add(25*time.Hour)); err != nil || n != 1 {
		t.Fatalf("sweep after 24h = %d, %v; want 1 summary", n, err)
	}
	got := rf.lecturerVerdictNotices()
	if len(got) != 1 {
		t.Fatalf("lecturer got %d notices, want the 24-hour summary: %+v", len(got), got)
	}
	if b := got[0].body; !strings.Contains(b, "ครบ 24 ชั่วโมง") || !strings.Contains(b, "อยู่ระหว่างพิจารณา 1 คน") ||
		!strings.Contains(b, "รอผู้ช่วยสอนบันทึกตารางเรียน") {
		t.Errorf("summary should say who passed and who is still waiting and why:\n%s", b)
	}
	// Sweeping again sends nothing new.
	if n, _ := rf.Req.SweepLecturerDigests(rf.ctx, time.Now().Add(26*time.Hour)); n != 0 {
		t.Errorf("second sweep sent %d, want 0", n)
	}

	// The last TA is decided later: told as a follow-up straight away.
	rf.addClassFor(second)
	if err := rf.Req.ReevaluateForTA(rf.ctx, second, rf.TermID); err != nil {
		t.Fatalf("reevaluate: %v", err)
	}
	got = rf.lecturerVerdictNotices()
	if len(got) != 2 {
		t.Fatalf("lecturer got %d notices, want summary + follow-up: %+v", len(got), got)
	}
	if !strings.Contains(got[1].title, "เพิ่มเติม") || strings.Contains(got[1].body, "อยู่ระหว่างพิจารณา") {
		t.Errorf("follow-up = %+v", got[1])
	}
}
