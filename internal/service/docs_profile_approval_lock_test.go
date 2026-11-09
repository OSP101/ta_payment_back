package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestUpsertProfile_ApprovedCreditorFormLocksItsSourceDetails(t *testing.T) {
	for _, status := range []string{"submitted", "needs_fix", "rejected", "approved"} {
		t.Run(status, func(t *testing.T) {
			svc, makeUser := profileSvc(t)
			ctx := context.Background()
			uid := makeUser("นาย", "ทดสอบ", "ทีเอ", "653020111-1", "0812345678")
			if err := svc.RecordPdpaConsent(ctx, uid, "127.0.0.1", "test-agent"); err != nil {
				t.Fatal(err)
			}
			in := TAProfile{
				StudentID: "653020111-1", Prefix: "นาย", Phone: "0812345678",
				NationalID: "1234567890121", BankName: "ธนาคารไทยพาณิชย์",
				BankBranch: "สาขามหาวิทยาลัยขอนแก่น", BranchCode: "1234",
				AccountNo: "4091290303", AccountName: "นาย ทดสอบ ทีเอ",
				AddressLine: "123 ม.16", SubDistrictID: 400101,
				SignatureSVG: "<svg><path d='M0 0 L9 9'/></svg>",
			}
			if err := svc.UpsertProfile(ctx, uid, in); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.pool.Exec(ctx, `INSERT INTO ta_documents
				(id,user_id,kind,filename,mime,size_bytes,storage_key,status,round)
				VALUES ($1,$2,'creditor_form','approved.pdf','application/pdf',1,'test/approved','approved',1)`, uuid.New(), uid); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.pool.Exec(ctx, `UPDATE ta_profiles SET status=$2::doc_status WHERE user_id=$1`, uid, status); err != nil {
				t.Fatal(err)
			}
			// A direct API caller or stale tab must not modify even the user's
			// ordinary fields or move the workflow to another round.
			changed := in
			changed.StudentID = "653020222-2"
			changed.Phone = "0899999999"
			changed.Prefix = "นางสาว"
			if err := svc.UpsertProfile(ctx, uid, changed); userErrStatus(err) != 409 {
				t.Fatalf("want conflict, got %v", err)
			}
			var sid, phone, prefix, actualStatus string
			var round int
			if err := svc.pool.QueryRow(ctx, `SELECT u.student_id,u.phone,p.prefix,p.status::text,p.current_round
				FROM users u JOIN ta_profiles p ON p.user_id=u.id WHERE u.id=$1`, uid).Scan(&sid, &phone, &prefix, &actualStatus, &round); err != nil {
				t.Fatal(err)
			}
			if sid != in.StudentID || phone != in.Phone || prefix != in.Prefix || actualStatus != status || round != 1 {
				t.Fatalf("locked details changed: %s %s %s %s round=%d", sid, phone, prefix, actualStatus, round)
			}
			if status == "approved" {
				return // Fully approved profiles remain locked as well.
			}
			// Staff must return THIS document before its source form can change.
			if _, err := svc.pool.Exec(ctx, `UPDATE ta_documents SET status='rejected' WHERE user_id=$1 AND kind='creditor_form'`, uid); err != nil {
				t.Fatal(err)
			}
			if err := svc.UpsertProfile(ctx, uid, changed); err != nil {
				t.Fatalf("returned creditor form must allow corrections: %v", err)
			}
		})
	}
}
