package service

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// Foreign TAs (06/10/2026, the office's rule): the creditor form's ID box takes
// the passport number, and the passport copy replaces the citizen-ID copy.

func makeForeign(t *testing.T, svc *DocsService, ta uuid.UUID) {
	t.Helper()
	if _, err := svc.pool.Exec(context.Background(),
		`UPDATE users SET nationality = 'foreign' WHERE id = $1`, ta); err != nil {
		t.Fatal(err)
	}
}

func TestValidateIDNumber(t *testing.T) {
	const badChecksum = "1234567890123" // 13 digits, check digit wrong
	if _, err := validateIDNumber(badChecksum, false); err == nil {
		t.Error("a Thai citizen ID with a wrong check digit must be refused")
	}
	got, err := validateIDNumber(" ab 123-4567 ", true)
	if err != nil || got != "AB1234567" {
		t.Errorf("passport number = %q, %v; want AB1234567", got, err)
	}
	for _, bad := range []string{"AB12", "ABCDEFGHIJ", "1234567890123", "AB12345678901"} {
		if _, err := validateIDNumber(bad, true); err == nil {
			t.Errorf("passport number %q must be refused", bad)
		}
	}
	if _, err := validateIDNumber("AB1234567", false); err == nil {
		t.Error("a Thai TA cannot give a passport number")
	}
}

func TestForeignTA_OwesPassportNotCitizenID(t *testing.T) {
	svc := completenessSvc(t)

	// A foreign TA holding exactly the Thai set is still missing the passport.
	thaiSet := docReviewFixture(t, svc.pool, "ไทยชุด", requiredDocKinds...)
	makeForeign(t, svc, thaiSet)
	full := docReviewFixture(t, svc.pool, "ครบชุด", foreignRequiredDocKinds...)
	makeForeign(t, svc, full)

	incomplete := listIDs(t, svc, "incomplete")
	row, ok := incomplete[thaiSet]
	if !ok {
		t.Fatal("foreign TA with only the Thai set must be in the incomplete bucket")
	}
	if row.DocsIn != 2 || row.DocsNeeded != 3 || !row.Foreign {
		t.Errorf("incomplete row = %d/%d foreign=%v, want 2/3 foreign", row.DocsIn, row.DocsNeeded, row.Foreign)
	}
	pending := listIDs(t, svc, "pending")
	if _, ok := pending[full]; !ok {
		t.Error("foreign TA with passport, bank book and creditor form must be in the review queue")
	}
	if _, ok := pending[thaiSet]; ok {
		t.Error("foreign TA missing the passport must not be in the review queue")
	}
}

func TestForeignTA_ProfileApprovedOnlyAfterLastDoc(t *testing.T) {
	svc := completenessSvc(t)
	ctx := context.Background()
	ta := docReviewFixture(t, svc.pool, "ต่างชาติ", foreignRequiredDocKinds...)
	makeForeign(t, svc, ta)
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO ta_profile_submissions (user_id, round, prefix, status) VALUES ($1, 1, 'นาย', 'submitted')`, ta); err != nil {
		t.Fatal(err)
	}
	officer := newTAUser(t, svc.pool, "เจ้าหน้าที่")

	for i, kind := range foreignRequiredDocKinds {
		var docID uuid.UUID
		if err := svc.pool.QueryRow(ctx,
			`SELECT id FROM ta_documents WHERE user_id = $1 AND kind = $2`, ta, kind).Scan(&docID); err != nil {
			t.Fatal(err)
		}
		if err := svc.Review(ctx, officer, docID, true, ""); err != nil {
			t.Fatalf("approve %s: %v", kind, err)
		}
		want := "submitted"
		if i == len(foreignRequiredDocKinds)-1 {
			want = "approved"
		}
		if got := profileStatus(t, svc, ta); got != want {
			t.Fatalf("after %d of %d, profile = %q, want %q", i+1, len(foreignRequiredDocKinds), got, want)
		}
	}
}

func TestUpload_RefusesKindTheTADoesNotOwe(t *testing.T) {
	svc := completenessSvc(t)
	thai := newTAUser(t, svc.pool, "ไทย")
	_, err := svc.Upload(context.Background(), thai, "passport", "p.pdf", "application/pdf", 4, bytes.NewReader([]byte("%PDF")))
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("a Thai TA uploading a passport must be refused with a user error, got %v", err)
	}
}

func TestNationality_SetAtCreateAndFrozenOnceApproved(t *testing.T) {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	users := &UserService{pool: f.Pool, aud: audit.New(f.Pool)}

	plain, err := users.Create(f.ctx, f.StaffID, CreateUserInput{
		Email: "nat-default@example.test", FirstName: "สมชาย", LastName: "ใจดี", Roles: []string{"ta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain.User.Nationality != "thai" {
		t.Errorf("omitted nationality = %q, want thai", plain.User.Nationality)
	}
	foreign := "foreign"
	out, err := users.Create(f.ctx, f.StaffID, CreateUserInput{
		Email: "nat-foreign@example.test", FirstName: "John", LastName: "Smith", Roles: []string{"ta"},
		Nationality: &foreign,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.User.Nationality != "foreign" {
		t.Fatalf("nationality = %q, want foreign", out.User.Nationality)
	}

	// Before approval staff may correct it.
	thai := "thai"
	if _, err := users.Update(f.ctx, f.StaffID, out.User.ID, UpdateUserInput{Nationality: &thai}); err != nil {
		t.Fatalf("change before approval: %v", err)
	}
	// After approval it is frozen: the approved files were the other set.
	if _, err := f.Pool.Exec(f.ctx,
		`INSERT INTO ta_profiles (user_id, prefix, status, current_round) VALUES ($1, 'นาย', 'approved', 1)`,
		out.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Update(f.ctx, f.StaffID, out.User.ID, UpdateUserInput{Nationality: &foreign}); !isConflict(err) {
		t.Fatalf("change after approval must be refused, got %v", err)
	}
	// Re-sending the current value is not a change and must not trip the guard.
	if _, err := users.Update(f.ctx, f.StaffID, out.User.ID, UpdateUserInput{Nationality: &thai}); err != nil {
		t.Fatalf("unchanged nationality after approval: %v", err)
	}
}

// The transfer cover's PromptPay column takes a foreign TA's passport number,
// through the same audited reveal as a Thai TA's citizen ID.
func TestTransferCover_ForeignTAPromptPayIsPassport(t *testing.T) {
	svc, ctx, ta := citizenIDFixture(t)
	makeForeign(t, svc, ta)
	tx, err := svc.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pp, err := validateIDNumber("ab1234567", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.storeCitizenID(ctx, tx, ta, pp); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	exp := &ExportService{pool: svc.pool, aud: svc.aud, docs: svc}
	sheets := []transferCoverSheet{{Rows: []transferCoverRow{{TAID: ta, Name: "นาย John Smith"}}}}
	if warnings := exp.fillPromptPay(ctx, ta, sheets); len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if got := sheets[0].Rows[0].PromptPay; got != "AB1234567" {
		t.Errorf("PromptPay = %q, want the passport number AB1234567", got)
	}
}
