package service

import (
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// 01/10/2026 — notices and hour precision.

func unreadTitles(f *fixture, user uuid.UUID) []string {
	f.t.Helper()
	rows, err := f.Pool.Query(f.ctx,
		`SELECT title FROM notifications WHERE user_id=$1 AND channel='in_app' AND read_at IS NULL ORDER BY created_at`, user)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, t)
	}
	return out
}

// Unread in-app notices with the same (title, link) are folded into one. The
// "waiting for approval" notice had a bare title and the course reports link,
// so the second TA's submission overwrote the first's.
func TestSubmit_LecturerNoticeNamesCourseTAAndMonth(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ta2 := f.secondTAOnSameCourse()
	var a2 uuid.UUID
	if err := f.Pool.QueryRow(f.ctx, `SELECT a.id FROM ta_request_assignments a WHERE a.ta_id=$1`, ta2).Scan(&a2); err != nil {
		t.Fatal(err)
	}
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2::date, '13:00', '14:00', 1, 'review', 'draft')`, a2, day(12))
	if err := f.Svc.Submit(f.ctx, ta2, a2); err != nil {
		t.Fatal(err)
	}
	titles := unreadTitles(f, f.LecturerID)
	if len(titles) != 2 {
		t.Fatalf("each TA's submission must stay its own notice, got %q", titles)
	}
	var code string
	_ = f.Pool.QueryRow(f.ctx, `SELECT code FROM teaching_courses WHERE id=$1`, f.CourseID).Scan(&code)
	month := thaiYearMonth(day(10)[:7])
	for _, ti := range titles {
		if !strings.Contains(ti, code) || !strings.Contains(ti, month) {
			t.Errorf("title must carry course and month, got %q", ti)
		}
	}
}

// The notice names the actor's real role, not the endpoint's.
func TestEditNotices_NameTheActorsRealRole(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	edited := f.entry(day(10), "09:00", "10:00", 1)
	edited.ID = id
	// A lecturer using the /staff/worklogs path.
	if _, err := f.Svc.StaffUpsert(f.ctx, f.LecturerID, false, edited, nil); err != nil {
		t.Fatal(err)
	}
	titles := unreadTitles(f, f.TAID)
	if len(titles) == 0 || !strings.HasPrefix(titles[len(titles)-1], "อาจารย์แก้ไข") {
		t.Fatalf("a lecturer's edit must be announced as อาจารย์, got %q", titles)
	}

	// An admin using the lecturer review screen.
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	admin := f.insertUser("admin", "admin")
	if err := f.Svc.LecturerCut(f.ctx, admin, true, id, "ไม่มีการเรียนการสอนวันนี้"); err != nil {
		t.Fatal(err)
	}
	titles = unreadTitles(f, f.TAID)
	last := titles[len(titles)-1]
	if strings.HasPrefix(last, "อาจารย์") || !strings.HasPrefix(last, "ผู้ดูแลระบบ") {
		t.Fatalf("an admin's cut must not be announced as อาจารย์, got %q", last)
	}
}

// A 23:00–23:59 row: hours and money must come from the same 59 minutes.
// Stored as 0.98 it priced 39.20 on screens and 39.33 on the claim form.
func TestHours_StoredExactlyFromMinutes(t *testing.T) {
	f := newFixture(t, fixtureOpts{Rates: rateOverrides{UndergradRegular: 40}})
	id := f.mustUpsert(f.entry(day(10), "23:00", "23:59", 0.98))
	var hours float64
	if err := f.Pool.QueryRow(f.ctx, `SELECT hours FROM work_logs WHERE id=$1`, id).Scan(&hours); err != nil {
		t.Fatal(err)
	}
	if got, want := math.Round(hours*40*100)/100, math.Round(59.0/60*40*100)/100; got != want {
		t.Fatalf("hours × rate = %.2f, minutes × rate = %.2f — they must agree (stored hours %v)", got, want, hours)
	}
	// A row whose hours genuinely differ from its clock range is left alone.
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2::date, '07:00', '08:00', 0.5, 'review', 'draft')`, f.AssignmentID, day(11))
	var odd float64
	_ = f.Pool.QueryRow(f.ctx, `SELECT hours FROM work_logs WHERE assignment_id=$1 AND work_date=$2::date`, f.AssignmentID, day(11)).Scan(&odd)
	if odd != 0.5 {
		t.Fatalf("only rounding is snapped; got %v", odd)
	}
}

// The TA course card shows the worst ACTIONABLE state: a bounced row beats
// approved hours (it read "อนุมัติแล้ว"), and an all-rejected course is not a
// draft (it read "แบบร่าง").
func TestTaOverview_StageShowsRejectedFirst(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	dash := &DashboardService{pool: f.Pool}
	stage := func() string {
		t.Helper()
		rows, err := dash.TaOverview(f.ctx, f.TAID, nil)
		if err != nil || len(rows) != 1 {
			t.Fatalf("TaOverview: %v (%d rows)", err, len(rows))
		}
		return rows[0].Stage
	}
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	f.exec(`UPDATE work_logs SET status='rejected', reject_reason='แก้เวลา' WHERE assignment_id=$1`, f.AssignmentID)
	if got := stage(); got != "rejected" {
		t.Fatalf("all-rejected course: stage %q, want rejected", got)
	}
	f.exec(`INSERT INTO work_logs (assignment_id, work_date, start_time, end_time, hours, activity, status)
	        VALUES ($1, $2::date, '13:00', '14:00', 1, 'review', 'approved')`, f.AssignmentID, day(11))
	if got := stage(); got != "rejected" {
		t.Fatalf("approved + rejected: stage %q, want rejected", got)
	}
}
