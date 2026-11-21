package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

func TestResetTADataValidation(t *testing.T) {
	for name, in := range map[string]ResetTADataInput{
		"empty": {Reason: "fix"}, "missing term": {Schedule: true, Reason: "fix"},
		"unknown document": {Documents: []string{"other"}, Reason: "fix"},
		"duplicate":        {Documents: []string{"bank_book", "bank_book"}, Reason: "fix"},
		"missing reason":   {Documents: []string{"creditor_form"}, Reason: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			if err := in.validate(); err == nil {
				t.Fatal("expected refusal")
			}
		})
	}
}

func TestResetTADataSelectiveAndAtomic(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	svc := &WorkloadService{pool: pool, aud: audit.New(pool)}
	staff := insertDashUser(t, pool, "staff")
	ta := insertDashUser(t, pool, "ta")
	lecturer := insertDashUser(t, pool, "lecturer")
	term := insertTerm(t, pool, 2569, 1, true)
	other := insertTerm(t, pool, 2569, 2, false)
	if _, err := pool.Exec(ctx, `INSERT INTO ta_profiles(user_id,status) VALUES($1,'approved')`, ta); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"creditor_form", "national_id", "bank_book"} {
		if _, err := pool.Exec(ctx, `INSERT INTO ta_documents(user_id,kind,filename,mime,size_bytes,storage_key,status) VALUES($1,$2,'test.pdf','application/pdf',10,$2,'approved')`, ta, kind); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uuid.UUID{term, other} {
		if _, err := pool.Exec(ctx, `INSERT INTO ta_class_schedules(user_id,term_id,day_of_week,start_time,end_time) VALUES($1,$2,1,'09:00','10:00')`, ta, id); err != nil {
			t.Fatal(err)
		}
	}
	in := ResetTADataInput{Documents: []string{"bank_book"}, Schedule: true, TermID: term, Reason: "แก้ไขข้อมูลผิดพลาด"}
	if err := svc.ResetTAData(ctx, lecturer, ta, in); err != ErrForbidden {
		t.Fatalf("lecturer: %v", err)
	}
	if err := svc.ResetTAData(ctx, staff, lecturer, in); err == nil {
		t.Fatal("non-TA target accepted")
	}
	bad := in
	bad.TermID = uuid.New()
	if err := svc.ResetTAData(ctx, staff, ta, bad); err == nil {
		t.Fatal("nonexistent term accepted")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ta_documents WHERE user_id=$1 AND superseded_at IS NULL`, ta).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("failed reset changed docs: %d", count)
	}
	if err := svc.ResetTAData(ctx, staff, ta, in); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ta_documents WHERE user_id=$1 AND superseded_at IS NULL`, ta).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("current docs: %d", count)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status::text FROM ta_profiles WHERE user_id=$1`, ta).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "needs_fix" {
		t.Fatalf("profile not reopened: %s", status)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2`, ta, term).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("chosen schedule retained")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2`, ta, other).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("other term removed")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ta_documents WHERE user_id=$1`, ta).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatal("document history lost")
	}
	if err := svc.ResetTAData(ctx, staff, ta, ResetTADataInput{Documents: []string{"creditor_form"}, Reason: "กรอกข้อมูลใหม่"}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM ta_profiles WHERE user_id=$1`, ta).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" {
		t.Fatalf("creditor form did not reopen profile step: %s", status)
	}
	if err := svc.ResetTAData(ctx, staff, ta, ResetTADataInput{Documents: []string{"national_id"}, Reason: "แก้ไขสำเนา"}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM ta_profiles WHERE user_id=$1`, ta).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" {
		t.Fatal("another reset closed the profile correction")
	}
}

func TestResetTADataExportedTermRollsBackDocuments(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := &WorkloadService{pool: f.Pool, aud: audit.New(f.Pool)}
	staff := f.insertUser("staff", "staff")
	period := insertDashPeriod(t, f.Pool, f.TermID, "2569-06")
	insertDashPeriodStatus(t, f.Pool, period, f.TAID, f.CourseID, "exported")
	if _, err := f.Pool.Exec(f.ctx, `INSERT INTO ta_profiles(user_id,status) VALUES($1,'approved') ON CONFLICT(user_id) DO UPDATE SET status='approved'`, f.TAID); err != nil {
		t.Fatal(err)
	}
	err := svc.ResetTAData(f.ctx, staff, f.TAID, ResetTADataInput{Schedule: true, TermID: f.TermID, Documents: []string{"bank_book"}, Reason: "fix"})
	if err == nil {
		t.Fatal("exported term accepted")
	}
	var status string
	if err := f.Pool.QueryRow(f.ctx, `SELECT status::text FROM ta_profiles WHERE user_id=$1`, f.TAID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "approved" {
		t.Fatal("profile changed on failed reset")
	}
}

func TestResetTADataAllowsResubmissionWithHistory(t *testing.T) {
	docs, _, ta := avFixture(t, nil)
	ctx := context.Background()
	staff := insertDashUser(t, docs.pool, "admin")
	if _, err := docs.pool.Exec(ctx, `INSERT INTO user_roles(user_id,role) VALUES($1,'ta');`, ta); err != nil {
		t.Fatal(err)
	}
	first, err := uploadPDF(docs, ta, "national_id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docs.pool.Exec(ctx, `INSERT INTO ta_profiles(user_id,status) VALUES($1,'approved');`, ta); err != nil {
		t.Fatal(err)
	}
	svc := &WorkloadService{pool: docs.pool, aud: docs.aud}
	if err := svc.ResetTAData(ctx, staff, ta, ResetTADataInput{Documents: []string{"national_id"}, Reason: "ส่งสำเนาผิด"}); err != nil {
		t.Fatal(err)
	}
	second, err := uploadPDF(docs, ta, "national_id")
	if err != nil {
		t.Fatalf("resubmit after reset: %v", err)
	}
	if first == second {
		t.Fatal("reset submission reused retired document")
	}
	var round int
	if err := docs.pool.QueryRow(ctx, `SELECT round FROM ta_documents WHERE id=$1 AND superseded_at IS NULL`, second).Scan(&round); err != nil {
		t.Fatal(err)
	}
	if round != 2 {
		t.Fatalf("round restarted: %d", round)
	}
	var retired bool
	if err := docs.pool.QueryRow(ctx, `SELECT superseded_at IS NOT NULL FROM ta_documents WHERE id=$1`, first).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	if !retired {
		t.Fatal("old document not preserved in history")
	}
	var count int
	if err := docs.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='ta_data.reset' AND entity_id=$1`, ta.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("missing audit: %d", count)
	}
}
