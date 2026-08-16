package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/testutil"
)

func mkDeletionTestUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1,$2,'ทดสอบ','ลบข้อมูล',TRUE)`,
		id, "deletion-"+id.String()+"@example.test"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func newDeletionSvc(t *testing.T, pool *pgxpool.Pool) *DataDeletionService {
	t.Helper()
	aud := audit.New(pool)
	return &DataDeletionService{
		pool:     pool,
		aud:      aud,
		docs:     &DocsService{pool: pool, aud: aud, store: newMemStore(), pii: testPIICipher(t)},
		users:    &UserService{pool: pool, aud: aud},
		sessions: &SessionService{pool: pool},
		notify:   nil, // nil-guarded in ReviewDeletion; tests don't need real sends
		store:    newMemStore(),
	}
}

// seedApprovedWorklog builds the minimal chain HasPaymentHistory's worklog
// EXISTS clause needs: term -> course -> ta_request -> section -> assignment
// -> one 'approved' work_log, all owned by taID.
func seedApprovedWorklog(t *testing.T, pool *pgxpool.Pool, taID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed worklog: %v\nSQL: %s", err, sql)
		}
	}
	term, lec, tc, req, sec, asg := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO academic_terms (id, academic_year, semester, is_active) VALUES ($1,$2,1,FALSE)`,
		term, 2500+int(taID[0]))
	exec(`INSERT INTO users (id,email,first_name,last_name,is_active) VALUES ($1,$2,'อาจารย์','ทดสอบ',TRUE)`,
		lec, "deletion-lec-"+lec.String()+"@example.test")
	exec(`INSERT INTO teaching_courses (id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs,num_students)
	      VALUES ($1,$2,$3,'วิชาทดสอบ','undergrad',3,2,2,5,40)`, tc, term, "DL"+tc.String()[:6])
	exec(`INSERT INTO teaching_lecturers (teaching_course_id,lecturer_id,is_primary) VALUES ($1,$2,TRUE)`, tc, lec)
	exec(`INSERT INTO ta_requests (id,teaching_course_id,lecturer_id,reimburse_scope,status,submitted_at)
	      VALUES ($1,$2,$3,'both','approved',NOW())`, req, tc, lec)
	exec(`INSERT INTO sections (id,teaching_course_id,sec_no,track) VALUES ($1,$2,'1','regular')`, sec, tc)
	exec(`INSERT INTO ta_request_assignments (id,request_id,section_id,ta_id,level)
	      VALUES ($1,$2,$3,$4,'undergrad')`, asg, req, sec, taID)
	exec(`INSERT INTO work_logs (id,assignment_id,work_date,start_time,end_time,hours,activity,status)
	      VALUES (gen_random_uuid(),$1,'2026-06-15','09:00'::time,'11:00'::time,2,'สอนปฏิบัติการ','approved')`, asg)
}

// seedAppointmentOrderItem exercises HasPaymentHistory's OTHER EXISTS clause
// independently of any worklog.
func seedAppointmentOrderItem(t *testing.T, pool *pgxpool.Pool, taID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed appointment order: %v\nSQL: %s", err, sql)
		}
	}
	term, tc, ord := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO academic_terms (id, academic_year, semester, is_active) VALUES ($1,$2,1,FALSE)`,
		term, 2600+int(taID[0]))
	exec(`INSERT INTO teaching_courses (id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs,num_students)
	      VALUES ($1,$2,$3,'วิชาทดสอบ','undergrad',3,2,2,5,40)`, tc, term, "AO"+tc.String()[:6])
	exec(`INSERT INTO appointment_orders (id,term_id,round_no,order_no,order_date,effective_date)
	      VALUES ($1,$2,1,'1/2569','1 มกราคม 2569','1 มกราคม 2569')`, ord, term)
	exec(`INSERT INTO appointment_order_items (appointment_order_id,teaching_course_id,ta_id)
	      VALUES ($1,$2,$3)`, ord, tc, taID)
}

func seedStoredCitizenID(t *testing.T, pool *pgxpool.Pool, svc *DataDeletionService, taID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO ta_profiles (user_id, status, current_round) VALUES ($1,'pending',1)`, taID); err != nil {
		t.Fatalf("insert ta_profiles: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.docs.storeCitizenID(ctx, tx, taID, "1234567890123"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHasPaymentHistory_FalseForFreshTA(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newDeletionSvc(t, pool)
	taID := mkDeletionTestUser(t, pool)

	ok, err := svc.HasPaymentHistory(context.Background(), taID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("HasPaymentHistory = true for a TA with no worklog or appointment history")
	}
}

func TestHasPaymentHistory_TrueViaApprovedWorklog(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newDeletionSvc(t, pool)
	taID := mkDeletionTestUser(t, pool)
	seedApprovedWorklog(t, pool, taID)

	ok, err := svc.HasPaymentHistory(context.Background(), taID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("HasPaymentHistory = false despite an approved work_log")
	}
}

func TestHasPaymentHistory_TrueViaAppointmentOrderItem(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newDeletionSvc(t, pool)
	taID := mkDeletionTestUser(t, pool)
	seedAppointmentOrderItem(t, pool, taID)

	ok, err := svc.HasPaymentHistory(context.Background(), taID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("HasPaymentHistory = false despite an appointment_order_items row")
	}
}

func TestRequestDeletion_OnePendingAtATime(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newDeletionSvc(t, pool)
	taID := mkDeletionTestUser(t, pool)
	ctx := context.Background()

	if err := svc.RequestDeletion(ctx, taID, "อยากให้ลบ"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if err := svc.RequestDeletion(ctx, taID, "อีกครั้ง"); err == nil {
		t.Error("expected the second pending request to fail (unique index violation)")
	}
}

func TestReviewDeletion_RejectRequiresNote(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newDeletionSvc(t, pool)
	admin := mkDeletionTestUser(t, pool)
	taID := mkDeletionTestUser(t, pool)
	ctx := context.Background()

	if err := svc.RequestDeletion(ctx, taID, ""); err != nil {
		t.Fatal(err)
	}
	var reqID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM data_deletion_requests WHERE user_id=$1`, taID).Scan(&reqID); err != nil {
		t.Fatal(err)
	}

	if err := svc.ReviewDeletion(ctx, admin, reqID, false, ""); err == nil {
		t.Error("expected reject without a note to be refused")
	}
}

func TestReviewDeletion_ApprovePartial_RetainsCitizenIDWhenPaymentHistoryExists(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newDeletionSvc(t, pool)
	admin := mkDeletionTestUser(t, pool)
	taID := mkDeletionTestUser(t, pool)
	ctx := context.Background()

	seedApprovedWorklog(t, pool, taID)
	seedStoredCitizenID(t, pool, svc, taID)
	if err := svc.RequestDeletion(ctx, taID, ""); err != nil {
		t.Fatal(err)
	}
	var reqID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM data_deletion_requests WHERE user_id=$1`, taID).Scan(&reqID); err != nil {
		t.Fatal(err)
	}

	if err := svc.ReviewDeletion(ctx, admin, reqID, true, ""); err != nil {
		t.Fatalf("ReviewDeletion approve: %v", err)
	}

	var isActive bool
	var deletedAt *string
	var citizenIDEnc []byte
	if err := pool.QueryRow(ctx,
		`SELECT u.is_active, u.deleted_at::text, p.citizen_id_enc
		 FROM users u JOIN ta_profiles p ON p.user_id = u.id WHERE u.id=$1`, taID,
	).Scan(&isActive, &deletedAt, &citizenIDEnc); err != nil {
		t.Fatal(err)
	}
	if isActive {
		t.Error("account should be deactivated after approved deletion")
	}
	if deletedAt != nil {
		t.Error("deleted_at should stay NULL when the TA has payment history (partial branch)")
	}
	if len(citizenIDEnc) == 0 {
		t.Error("citizen_id_enc should be RETAINED when the TA has payment history")
	}
}

func TestReviewDeletion_ApproveFull_ClearsCitizenIDAndSetsDeletedAtWhenNoPaymentHistory(t *testing.T) {
	pool := testutil.NewPool(t)
	svc := newDeletionSvc(t, pool)
	admin := mkDeletionTestUser(t, pool)
	taID := mkDeletionTestUser(t, pool)
	ctx := context.Background()

	seedStoredCitizenID(t, pool, svc, taID)
	if err := svc.RequestDeletion(ctx, taID, ""); err != nil {
		t.Fatal(err)
	}
	var reqID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM data_deletion_requests WHERE user_id=$1`, taID).Scan(&reqID); err != nil {
		t.Fatal(err)
	}

	if err := svc.ReviewDeletion(ctx, admin, reqID, true, ""); err != nil {
		t.Fatalf("ReviewDeletion approve: %v", err)
	}

	var isActive bool
	var deletedAt *string
	var citizenIDEnc []byte
	if err := pool.QueryRow(ctx,
		`SELECT u.is_active, u.deleted_at::text, p.citizen_id_enc
		 FROM users u JOIN ta_profiles p ON p.user_id = u.id WHERE u.id=$1`, taID,
	).Scan(&isActive, &deletedAt, &citizenIDEnc); err != nil {
		t.Fatal(err)
	}
	if isActive {
		t.Error("account should be deactivated after approved deletion")
	}
	if deletedAt == nil {
		t.Error("deleted_at should be set when the TA has no payment history (full branch)")
	}
	if len(citizenIDEnc) != 0 {
		t.Error("citizen_id_enc should be CLEARED when the TA has no payment history")
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM data_deletion_requests WHERE id=$1`, reqID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "approved" {
		t.Errorf("request status = %q, want approved", status)
	}
}
