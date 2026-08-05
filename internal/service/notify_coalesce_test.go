package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/testutil"
)

// A TA holding two sections of one course got the rejection twice, and again
// each time the lecturer bounced the batch — four identical lines about one
// thing. The bell became noise, which is how the notice that mattered went
// unread. Unread notices with the same title and link now collapse into one.

func notifyFixture(t *testing.T) (*NotifyService, uuid.UUID, context.Context) {
	t.Helper()
	pool := testutil.NewPool(t)
	ctx := context.Background()
	uid := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id,email,first_name,last_name,is_active)
		 VALUES ($1,$2,'ผู้ช่วย','ทดสอบ',TRUE)`, uid, "ta-"+uid.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	// A zero Mailer has no SMTP host configured, so Send() just logs — the email
	// leg stays out of the way while the in-app rows are what is under test.
	return &NotifyService{pool: pool, mailer: mail.New(config.Config{})}, uid, ctx
}

func (s *NotifyService) countFor(t *testing.T, ctx context.Context, uid uuid.UUID) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND channel='in_app'`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNotify_UnreadDuplicatesCollapseIntoOne(t *testing.T) {
	s, uid, ctx := notifyFixture(t)
	link := "/ta/courses/" + uuid.New().String() + "/worklog"

	s.Send(ctx, uid, "บันทึกเวลา CP321002 ถูกตีกลับ", "เหตุผลแรก", link)
	s.Send(ctx, uid, "บันทึกเวลา CP321002 ถูกตีกลับ", "เหตุผลที่สอง", link)
	s.Send(ctx, uid, "บันทึกเวลา CP321002 ถูกตีกลับ", "เหตุผลล่าสุด", link)

	if n := s.countFor(t, ctx, uid); n != 1 {
		t.Errorf("in-app rows = %d, want 1 — two sections of one course are one piece "+
			"of news, not two", n)
	}
	// The newest reason wins: an old reason on top of a fresh rejection tells the
	// TA to fix the wrong thing.
	var body string
	if err := s.pool.QueryRow(ctx,
		`SELECT body FROM notifications WHERE user_id=$1 AND channel='in_app'`, uid).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "เหตุผลล่าสุด" {
		t.Errorf("body = %q, want the latest reason", body)
	}
}

// Different courses are different news, even with the same kind of event.
func TestNotify_DifferentCoursesStaySeparate(t *testing.T) {
	s, uid, ctx := notifyFixture(t)
	s.Send(ctx, uid, "บันทึกเวลา CP321002 ถูกตีกลับ", "a", "/ta/courses/aaa/worklog")
	s.Send(ctx, uid, "บันทึกเวลา SC363001 ถูกตีกลับ", "b", "/ta/courses/bbb/worklog")

	if n := s.countFor(t, ctx, uid); n != 2 {
		t.Errorf("in-app rows = %d, want 2 — collapsing across courses would hide one", n)
	}
}

// Once the TA has read it, the next one is news again — otherwise a second
// rejection after they had dealt with the first would never reach them.
func TestNotify_ReadNoticesDoNotAbsorbNewOnes(t *testing.T) {
	s, uid, ctx := notifyFixture(t)
	link := "/ta/courses/ccc/worklog"
	s.Send(ctx, uid, "บันทึกเวลา CP321002 ถูกตีกลับ", "รอบแรก", link)
	if _, err := s.pool.Exec(ctx,
		`UPDATE notifications SET read_at = NOW() WHERE user_id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	s.Send(ctx, uid, "บันทึกเวลา CP321002 ถูกตีกลับ", "รอบสอง", link)

	if n := s.countFor(t, ctx, uid); n != 2 {
		t.Errorf("in-app rows = %d, want 2 — a rejection after the last one was read "+
			"must show up", n)
	}
}
