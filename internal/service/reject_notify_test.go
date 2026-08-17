package service

import (
	"strings"
	"testing"
)

// A rejection the TA cannot locate is not a notification.
//
// Every work-log notice used to read the same and link to /ta/worklog — a route
// that does not exist, for a screen that is per-course. A TA assisting three
// courses got four identical lines in the bell and no way to tell which course
// had been sent back, or to reach it.

// lastNotificationFor returns the newest notification row for the fixture's TA.
func (f *fixture) lastNotificationFor(t *testing.T) (title, body, link string) {
	t.Helper()
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT title, body, COALESCE(link,'')
		FROM notifications WHERE user_id = $1
		ORDER BY created_at DESC, id DESC LIMIT 1`, f.TAID).Scan(&title, &body, &link); err != nil {
		t.Fatalf("no notification was sent: %v", err)
	}
	return
}

func TestReject_NotifiesTheTAWithTheCourseAndAWorkingLink(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Reject(f.ctx, f.LecturerID, f.AssignmentID, "ชั่วโมงไม่ตรงตาราง", "", false); err != nil {
		t.Fatal(err)
	}

	var code string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT code FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&code); err != nil {
		t.Fatal(err)
	}

	title, body, link := f.lastNotificationFor(t)
	if !strings.Contains(title, code) {
		t.Errorf("title = %q — it must name the course, or a TA on three courses "+
			"cannot tell which one came back", title)
	}
	if !strings.Contains(body, "ชั่วโมงไม่ตรงตาราง") {
		t.Errorf("body = %q — the lecturer's reason is the only thing that says what to fix", body)
	}
	// The link must reach the page that can actually be acted on.
	want := "/ta/courses/" + f.CourseID.String() + "/worklog"
	if link != want {
		t.Errorf("link = %q, want %q — /ta/worklog is not a route that exists", link, want)
	}
}

func TestApprove_NotifiesWithTheCourseToo(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false); err != nil {
		t.Fatal(err)
	}

	_, _, link := f.lastNotificationFor(t)
	if !strings.HasPrefix(link, "/ta/courses/") {
		t.Errorf("link = %q, want a per-course worklog link", link)
	}
}

// The month is printed the way the rest of the UI prints months. "2026-09" in a
// Thai notification is a leaked database format.
func TestThaiYearMonth(t *testing.T) {
	cases := map[string]string{
		"2026-09": "กันยายน 2569",
		"2026-01": "มกราคม 2569",
		"":        "",
		"2026-13": "",
		"garbage": "",
	}
	for in, want := range cases {
		if got := thaiYearMonth(in); got != want {
			t.Errorf("thaiYearMonth(%q) = %q, want %q", in, got, want)
		}
	}
}

// The rejected rows must be countable on their own — folded into "waiting on the
// TA" they read as work never started.
func TestPeriodStatus_RejectedIsCountedSeparately(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.seedPeriod(t, day(10)[5:7])
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	if err := f.Svc.Reject(f.ctx, f.LecturerID, f.AssignmentID, "แก้ด้วย", "", false); err != nil {
		t.Fatal(err)
	}

	svc := &SubmissionPeriodService{pool: f.Pool}
	rows, err := svc.PendingByTA(f.ctx, f.TAID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rejected, waitingTA int
	for _, r := range rows {
		if r.TeachingCourseID == f.CourseID {
			rejected += r.WorklogRejected
			waitingTA += r.WorklogWaitingTA
		}
	}
	if rejected != 1 {
		t.Errorf("rejected = %d, want 1", rejected)
	}
	if waitingTA != 1 {
		t.Errorf("waiting TA = %d, want 1 — a bounced row is still the TA's move", waitingTA)
	}
}
