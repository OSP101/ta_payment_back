package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

// The profile form must arrive pre-filled with what the system already knows
// (24/07/2026 meeting). Before this, a TA opening it for the first time had no
// ta_profiles row, the inner join failed, and every field — including the
// student id and phone staff had already entered — came back blank.

func profileSvc(t *testing.T) (*DocsService, func(title, first, last, studentID, phone string) uuid.UUID) {
	t.Helper()
	pool := testutil.NewPool(t)
	svc := &DocsService{pool: pool, aud: audit.New(pool), store: newMemStore(), pii: testPIICipher(t)}
	ctx := context.Background()

	makeUser := func(title, first, last, studentID, phone string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO users (id, email, title, first_name, last_name, student_id, phone, is_active)
			VALUES ($1,$2,$3,$4,$5,$6,$7,TRUE)`,
			id, "profile-"+id.String()+"@example.test", title, first, last, studentID, phone); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		return id
	}
	return svc, makeUser
}

func TestGetProfile_PrefillsForFirstTimeTA(t *testing.T) {
	svc, makeUser := profileSvc(t)
	uid := makeUser("นาย", "วรพจน์", "สุวรรณภิภพ", "653020123-4", "0812345678")

	p, err := svc.GetProfile(context.Background(), uid)
	if err != nil {
		t.Fatalf("GetProfile with no ta_profiles row must still succeed: %v", err)
	}
	if p.StudentID != "653020123-4" {
		t.Errorf("student_id = %q, want it carried over from users", p.StudentID)
	}
	if p.Phone != "0812345678" {
		t.Errorf("phone = %q, want it carried over from users", p.Phone)
	}
	if p.Prefix != "นาย" {
		t.Errorf("prefix = %q, want นาย", p.Prefix)
	}
	if p.AccountName != "นาย วรพจน์ สุวรรณภิภพ" {
		t.Errorf("account_name = %q, want the TA's own name", p.AccountName)
	}
	if p.Status != "pending" {
		t.Errorf("status = %q, want pending for a profile that does not exist yet", p.Status)
	}
	// Never guessed — an empty box beats a wrong number on a payment form.
	if p.NationalID != "" || p.BankName != "" || p.AccountNo != "" {
		t.Errorf("bank/national-id fields must stay empty, got %+v", p)
	}
}

// users.title also holds academic titles, which the creditor form's prefix
// circles cannot render. Those must not leak into the prefix field.
func TestGetProfile_IgnoresNonFormPrefix(t *testing.T) {
	svc, makeUser := profileSvc(t)
	uid := makeUser("ผศ. ดร.", "งามนิจ", "อาจอินทร์", "", "")

	p, err := svc.GetProfile(context.Background(), uid)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if p.Prefix != "" {
		t.Errorf("prefix = %q, want empty only นาย/นาง/นางสาว may seed it", p.Prefix)
	}
	// The name still seeds the account name, just without a prefix in front.
	if p.AccountName != "งามนิจ อาจอินทร์" {
		t.Errorf("account_name = %q, want the bare name", p.AccountName)
	}
}

// A saved profile is the authority — seeding must never overwrite it.
// Only the non-sensitive fields are still stored, so only those can be tested:
// the bank and ID columns are gone (PDPA, migration 0047).
func TestGetProfile_SavedValuesWin(t *testing.T) {
	svc, makeUser := profileSvc(t)
	ctx := context.Background()
	uid := makeUser("นาย", "วรพจน์", "สุวรรณภิภพ", "653020123-4", "0812345678")

	if _, err := svc.pool.Exec(ctx, `
		INSERT INTO ta_profiles (user_id, prefix, status)
		VALUES ($1, 'นางสาว', 'submitted')`,
		uid); err != nil {
		t.Fatalf("insert profile: %v", err)
	}

	p, err := svc.GetProfile(ctx, uid)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if p.Prefix != "นางสาว" {
		t.Errorf("prefix = %q, want the saved value", p.Prefix)
	}
	if p.Status != "submitted" {
		t.Errorf("status = %q, want submitted", p.Status)
	}
}

func TestGetProfile_UnknownUser(t *testing.T) {
	svc, _ := profileSvc(t)
	if _, err := svc.GetProfile(context.Background(), uuid.New()); err == nil {
		t.Fatal("a user that does not exist must still be an error")
	}
}

// PDPA (policy 2026-07-29, amended 09/08/2026): bank details and the
// signature are never written to any table — they arrive in the request, are
// drawn onto the creditor-form PDF, and are gone. The national ID is the ONE
// deliberate exception (migration 0076): the ปะหน้าจ่ายตรง document needs it,
// so it is now stored, but only as an XChaCha20-Poly1305 ciphertext plus a
// last4 plaintext for display — never as the literal 13-digit column
// (national_id/national_id_provided_at) migration 0047 dropped, and never
// handed back out of GetProfile.
//
// This pins the property directly against the database, because the earlier
// bug in this area was exactly a mismatch between what the form collected and
// what a later read expected to find: a submitted profile stored a national ID
// that a PDPA scrub then removed, and every completeness check broke.
func TestUpsertProfile_StoresNothingSensitive(t *testing.T) {
	svc, makeUser := profileSvc(t)
	ctx := context.Background()
	uid := makeUser("นาย", "สุพพิธาน", "ภักสวัสดิ์", "653020111-1", "0812345678")

	if err := svc.UpsertProfile(ctx, uid, TAProfile{
		StudentID: "653020111-1", Prefix: "นาย", Phone: "0812345678",
		NationalID: "1-2345-67890-12-3",
		BankName:   "ธนาคารไทยพาณิชย์", BankBranch: "สาขามหาวิทยาลัยขอนแก่น",
		BranchCode: "1234", AccountNo: "4091290303", AccountName: "นาย สุพพิธาน ภักสวัสดิ์",
		SignatureSVG: "<svg><path d='M0 0 L9 9'/></svg>", SignaturePNGB64: "iVBORw0KGgo=",
	}); err != nil {
		t.Fatalf("UpsertProfile: %v", err)
	}

	// The bank/signature columns — and the OLD plaintext national_id column —
	// must not exist at all. A NULL column would still be a place for a
	// future writer to put the number back without anyone noticing.
	for _, table := range []string{"ta_profiles", "ta_profile_submissions"} {
		var cols []string
		rows, err := svc.pool.Query(ctx, `
			SELECT column_name FROM information_schema.columns
			WHERE table_name = $1
			  AND column_name IN ('national_id','national_id_provided_at','bank_name',
			                      'bank_branch','branch_code','account_no','account_name',
			                      'signature_svg','signature_png_b64')`, table)
		if err != nil {
			t.Fatalf("introspect %s: %v", table, err)
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			cols = append(cols, c)
		}
		rows.Close()
		if len(cols) > 0 {
			t.Errorf("%s still has PDPA columns %v ข้อมูลสำคัญต้องไม่ถูกเก็บลงฐานข้อมูล", table, cols)
		}
	}

	// The national ID DOES get stored now — encrypted. Confirms the exception
	// actually took effect, not just that the old column stayed gone.
	var enc []byte
	var last4 string
	if err := svc.pool.QueryRow(ctx,
		`SELECT citizen_id_enc, citizen_id_last4 FROM ta_profiles WHERE user_id=$1`, uid,
	).Scan(&enc, &last4); err != nil {
		t.Fatalf("citizen_id columns: %v", err)
	}
	if len(enc) == 0 {
		t.Error("citizen_id_enc was not written")
	}
	// NationalID is stripped to digits before storage (see validateProfileInput)
	// — "1-2345-67890-12-3" becomes "1234567890123", so last4 is "0123".
	if last4 != "0123" {
		t.Errorf("citizen_id_last4 = %q, want 0123 (digits-only form of ...12-3)", last4)
	}

	// The workflow state IS recorded, so the checklist can tell a submitted
	// profile from an untouched one without reading any of the values.
	p, err := svc.GetProfile(ctx, uid)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if p.Status != "submitted" {
		t.Errorf("status = %q, want submitted ขั้นที่ 1 ต้องนับว่าเสร็จ", p.Status)
	}
	// GetProfile must still never hand the national ID (or anything else
	// sensitive) back out, even though it is now stored somewhere.
	if p.NationalID != "" || p.AccountNo != "" || p.SignatureSVG != "" {
		t.Errorf("GetProfile leaked sensitive values back: nid=%q acct=%q sig=%q",
			p.NationalID, p.AccountNo, p.SignatureSVG)
	}
	// Non-sensitive prefill still works — that is the whole reason the form is
	// pre-populated at all.
	if p.StudentID != "653020111-1" || p.Prefix != "นาย" {
		t.Errorf("prefill lost: student_id=%q prefix=%q", p.StudentID, p.Prefix)
	}
}
