package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A TA whose account was deactivated used to stay on the คำสั่งแต่งตั้ง: the
// roster only looked at the request, never the person. They must be left off
// the printed order AND named in the preview with the reason.
func TestAppointment_DeactivatedTASkippedWithReason(t *testing.T) {
	f := newApptFixture(t)
	_, active := f.addCourseWithTA("CP100", "ทำงาน", "approved")
	tcGone, gone := f.addCourseWithTA("CP200", "ลาออก", "approved")
	if _, err := f.svc.pool.Exec(f.ctx, `UPDATE users SET is_active = FALSE WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}

	p, err := f.svc.Preview(f.ctx, f.term)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Include) != 1 || p.Include[0].TAID != active {
		t.Fatalf("include = %+v, want only the active TA", p.Include)
	}
	if len(p.SkippedTAs) != 1 || p.SkippedTAs[0].TAID != gone || p.SkippedTAs[0].TeachingCourseID != tcGone ||
		!strings.HasPrefix(p.SkippedTAs[0].Reason, "ข้าม:") {
		t.Fatalf("skipped_tas = %+v, want the deactivated TA with a ข้าม: reason", p.SkippedTAs)
	}
	if n, err := f.svc.PendingCount(f.ctx, f.term); err != nil || n != 1 {
		t.Fatalf("PendingCount = %d, %v; want 1", n, err)
	}

	if _, _, err := f.svc.Build(f.ctx, uuid.Nil, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	var printed int
	if err := f.svc.pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM appointment_order_items WHERE ta_id = $1`, gone).Scan(&printed); err != nil {
		t.Fatal(err)
	}
	if printed != 0 {
		t.Fatal("deactivated TA was printed on the order")
	}
}

// Staff/lecturer work-log writes in a deactivated TA's name are refused (the
// handler calls this before WorkLogService.StaffUpsert).
func TestEnsureAssignmentTAActive(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := taReqSvcFor(f)
	if err := svc.EnsureAssignmentTAActive(f.ctx, uuid.Nil, f.AssignmentID); err != nil {
		t.Fatalf("active TA refused: %v", err)
	}
	f.exec(`UPDATE users SET is_active = FALSE WHERE id = $1`, f.TAID)
	wantUserErr(t, svc.EnsureAssignmentTAActive(f.ctx, uuid.Nil, f.AssignmentID), 409)
}
