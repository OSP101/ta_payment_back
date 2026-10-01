package service

import (
	"strings"
	"testing"

	"ta-payment-back/internal/audit"
)

// A lecturer cancelling an APPROVED request used to tell nobody: the TA, who
// had been sent an appointment notice, only found out when the worklog page
// answered "คำขอ TA ยังไม่ได้รับการอนุมัติ" — as if approval were still coming.
func TestCancel_ApprovedRequestNotifiesTAAndWorklogSaysCancelled(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := &TARequestService{pool: f.Pool, aud: audit.New(f.Pool), notify: f.Svc.notify}
	var code string
	if err := f.Pool.QueryRow(f.ctx, `SELECT code FROM teaching_courses WHERE id = $1`, f.CourseID).Scan(&code); err != nil {
		t.Fatal(err)
	}

	if err := svc.Cancel(f.ctx, f.LecturerID, f.RequestID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	var title, body string
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT title, body FROM notifications WHERE user_id = $1 AND channel = 'in_app'`, f.TAID).
		Scan(&title, &body); err != nil {
		t.Fatalf("TA got no cancellation notice: %v", err)
	}
	if !strings.Contains(title, "ยกเลิก") || !strings.Contains(title, code) {
		t.Errorf("title = %q, want the cancellation and the course code %s", title, code)
	}

	_, err := f.Svc.Upsert(f.ctx, f.TAID, f.entry(day(10), "09:00", "11:00", 2))
	if err == nil {
		t.Fatal("worklog write against a cancelled request was accepted")
	}
	if !strings.Contains(err.Error(), "ยกเลิก") || strings.Contains(err.Error(), "ยังไม่ได้รับการอนุมัติ") {
		t.Errorf("worklog refusal = %q, want it to say the appointment was cancelled", err)
	}
}

// Cancel's own refusals carry real statuses now that the handler passes them
// through ErrorHandler instead of wrapping everything as a 400.
func TestCancel_RefusalsAreTypedUserErrors(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := taReqSvcFor(f)
	wantUserErr(t, svc.Cancel(f.ctx, f.LecturerID, f.TAID /* not a request id */), 404)
	wantUserErr(t, svc.Cancel(f.ctx, f.TAID, f.RequestID), 403)
}

// The duplicate hint used to say "cancel the old request first", but Cancel
// refuses most of those cases. It must point at adding sections instead.
func TestCreate_DuplicateHintPointsAtAddingSections(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	_, err := taReqSvcFor(f).Create(f.ctx, f.LecturerID, inputForFixtureTA(f))
	if err == nil {
		t.Fatal("duplicate accepted")
	}
	if strings.Contains(err.Error(), "ยกเลิกคำขอเดิม") || !strings.Contains(err.Error(), "เพิ่ม section") {
		t.Errorf("hint = %q", err)
	}
	wantUserErr(t, err, 409)
}
