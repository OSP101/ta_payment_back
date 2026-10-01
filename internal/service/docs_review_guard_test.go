package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Officer-reachable refusals in Review/ReviewProfile used to be English
// errors.New values, which the error handler turns into a generic 500. Each
// must now be a Thai UserError with a 4xx status.

func wantUserErr(t *testing.T, err error, status int) {
	t.Helper()
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *UserError %d", err, err, status)
	}
	if ue.Status != status {
		t.Fatalf("status = %d (%q), want %d", ue.Status, ue.Msg, status)
	}
	if !containsThaiRune(ue.Msg) {
		t.Fatalf("message %q is not Thai", ue.Msg)
	}
}

func containsThaiRune(s string) bool {
	for _, r := range s {
		if r >= 0x0E00 && r <= 0x0E7F {
			return true
		}
	}
	return false
}

func reviewOfficer(t *testing.T, svc *DocsService) uuid.UUID {
	t.Helper()
	officer := uuid.New()
	if _, err := svc.pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name, is_active)
		 VALUES ($1, $2, 'จ.น.', 'ทดสอบ', TRUE)`,
		officer, "guard-officer-"+officer.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	return officer
}

func TestReview_UserFacingRefusalsAreThai4xx(t *testing.T) {
	svc, _, docs := autoApproveFixture(t)
	ctx := context.Background()
	officer := reviewOfficer(t, svc)

	// Whitespace-only reason.
	wantUserErr(t, svc.Review(ctx, officer, docs["national_id"], false, "   "), 400)
	// A document id that does not exist.
	wantUserErr(t, svc.Review(ctx, officer, uuid.New(), true, ""), 404)
	// An old tab acting on a superseded document.
	if _, err := svc.pool.Exec(ctx, `UPDATE ta_documents SET superseded_at = NOW() WHERE id = $1`, docs["bank_book"]); err != nil {
		t.Fatal(err)
	}
	wantUserErr(t, svc.Review(ctx, officer, docs["bank_book"], true, ""), 409)

	// ReviewProfile: missing profile, whitespace reason.
	wantUserErr(t, svc.ReviewProfile(ctx, officer, uuid.New(), true, ""), 404)
	wantUserErr(t, svc.ReviewProfile(ctx, officer, uuid.New(), false, " \t"), 400)
}

// A rejected document must not be approved in place: the TA has to upload a
// replacement first (which supersedes the rejected row).
func TestReview_CannotApproveRejectedDocument(t *testing.T) {
	svc, ta, docs := autoApproveFixture(t)
	ctx := context.Background()
	officer := reviewOfficer(t, svc)

	if err := svc.Review(ctx, officer, docs["national_id"], false, "ไฟล์ไม่ชัด"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	wantUserErr(t, svc.Review(ctx, officer, docs["national_id"], true, ""), 409)

	var st string
	if err := svc.pool.QueryRow(ctx, `SELECT status::text FROM ta_documents WHERE id=$1`, docs["national_id"]).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "rejected" {
		t.Fatalf("doc status = %q, want still rejected", st)
	}

	// Approve-all must refuse the same way rather than sweeping it back.
	if _, err := svc.ApproveAll(ctx, officer, ta); err == nil {
		t.Fatal("ApproveAll approved a set containing a rejected document")
	} else {
		wantUserErr(t, err, 409)
	}
}
