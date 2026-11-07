package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

// Exercise the real HTTP -> services -> storage path for a graduate-special
// monthly reprint: no work logs or submission-status rows exist at all.
func TestMonthlyPayoutReprintsGraduateOnlyArchive(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), audit.New(pool), config.Config{}, nil, nil)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := pool.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	term, course, section, request, ta, actor := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{ta, actor} {
		exec(`INSERT INTO users(id,email,first_name,last_name,study_level) VALUES($1,$2,'test','monthly','master')`, id, id.String()+"@example.test")
	}
	exec(`INSERT INTO user_roles(user_id,role) VALUES($1,'staff')`, actor)
	exec(`INSERT INTO academic_terms(id,academic_year,semester,starts_on,ends_on) VALUES($1,2569,1,'2026-06-01','2026-07-31')`, term)
	exec(`INSERT INTO submission_periods(id,term_id,year_month,label,starts_on,due_date,is_closed) VALUES(gen_random_uuid(),$1,'2569-06','มิถุนายน 2569','2026-06-01','2026-06-30',true)`, term)
	exec(`INSERT INTO teaching_courses(id,term_id,code,name_th,level,credits,lecture_hrs,lab_hrs,self_hrs) VALUES($1,$2,'CPTEST','test','graduate',3,3,0,0)`, course, term)
	exec(`INSERT INTO sections(id,teaching_course_id,sec_no,track) VALUES($1,$2,'1','special')`, section, course)
	exec(`INSERT INTO ta_requests(id,teaching_course_id,lecturer_id,reimburse_scope,status) VALUES($1,$2,$3,'both','approved')`, request, course, actor)
	exec(`INSERT INTO ta_request_assignments(id,request_id,section_id,ta_id,level) VALUES(gen_random_uuid(),$1,$2,$3,'master')`, request, section, ta)
	exec(`INSERT INTO grad_lump_ledger(teaching_course_id,ta_id,year_month,baht,lump_basis) VALUES($1,$2,'2026-06',500,1000)`, course, ta)
	body := []byte("exact original combined archive, never rebuilt")
	coursePath, _, err := store.Save("export_batches", "course.zip", bytes.NewReader([]byte("course archive")))
	if err != nil {
		t.Fatal(err)
	}
	monthPath, _, err := store.Save("monthly_payout_batches", "month.zip", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO export_batches(id,teaching_course_id,file_path,file_name,ta_count,total_baht,generated_by,months) VALUES(gen_random_uuid(),$1,$2,'course.zip',1,500,$3,ARRAY['2026-06'])`, course, coursePath, actor)
	exec(`INSERT INTO monthly_payout_batches(term_id,year_month,version,file_path,file_name,course_ids,total_baht,generated_by) VALUES($1,'2026-06',1,$2,'month.zip',$3,500,$4)`, term, monthPath, []uuid.UUID{course}, actor)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	h := &ExportHandler{Svc: svc}
	app.Post("/exports/terms/:id/monthly-payout.zip", func(c *fiber.Ctx) error { c.Locals(string(CtxUserID), actor); return c.Next() }, h.MonthlyPayoutZip)

	// Another month of the same term shares the budget. Its issuer must not
	// build a second snapshot while this term-wide lock is held.
	lockConn, e := pool.Acquire(ctx)
	if e != nil {
		t.Fatal(e)
	}
	key := "monthly-payout/" + term.String()
	if _, e = lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, key); e != nil {
		t.Fatal(e)
	}
	busy, e := app.Test(httptest.NewRequest("POST", "/exports/terms/"+term.String()+"/monthly-payout.zip?months=2026-06", nil), -1)
	if e != nil {
		t.Fatal(e)
	}
	busy.Body.Close()
	if _, e = lockConn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, key); e != nil {
		t.Fatal(e)
	}
	lockConn.Release()
	if busy.StatusCode != 409 {
		t.Fatalf("concurrent issue status=%d want409", busy.StatusCode)
	}
	response, err := app.Test(httptest.NewRequest("POST", "/exports/terms/"+term.String()+"/monthly-payout.zip?months=2026-06", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !bytes.Equal(got, body) {
		t.Fatalf("status=%d body=%s", response.StatusCode, got)
	}
	var versions int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM monthly_payout_batches`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatal("reprint created a revision")
	}

	// A later roster change must not make an already-issued archive disappear.
	exec(`UPDATE ta_request_assignments SET state='dropped' WHERE ta_id=$1`, ta)
	dropped, e := app.Test(httptest.NewRequest("POST", "/exports/terms/"+term.String()+"/monthly-payout.zip?months=2026-06", nil), -1)
	if e != nil {
		t.Fatal(e)
	}
	droppedBody, _ := io.ReadAll(dropped.Body)
	dropped.Body.Close()
	if dropped.StatusCode != 200 || !bytes.Equal(droppedBody, body) {
		t.Fatalf("issued archive disappeared after roster change: status=%d body=%s", dropped.StatusCode, droppedBody)
	}
	exec(`UPDATE ta_request_assignments SET state='active' WHERE ta_id=$1`, ta)
	// A stale client cannot combine this with another month.
	bad, err := app.Test(httptest.NewRequest("POST", "/exports/terms/"+term.String()+"/monthly-payout.zip?months=2026-06,2026-07", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Fatalf("multi-month status=%d", bad.StatusCode)
	}
	// Recover/finalize a month whose course archives already exist. This runs
	// the real combined ZIP builder, manifest, signer snapshot and atomic commit.
	exec(`DELETE FROM monthly_payout_batches WHERE term_id=$1`, term)
	fresh, e := app.Test(httptest.NewRequest("POST", "/exports/terms/"+term.String()+"/monthly-payout.zip?months=2026-06", nil), -1)
	if e != nil {
		t.Fatal(e)
	}
	defer fresh.Body.Close()
	generated, _ := io.ReadAll(fresh.Body)
	if fresh.StatusCode != 200 {
		t.Fatalf("issue status=%d body=%s", fresh.StatusCode, generated)
	}
	archive, e := zip.NewReader(bytes.NewReader(generated), int64(len(generated)))
	if e != nil {
		t.Fatal(e)
	}
	if len(archive.File) != 2 {
		t.Fatalf("combined pack entries=%d", len(archive.File))
	}
	overview, e := svc.DocProgress.GetOverview(ctx, term)
	if e != nil {
		t.Fatal(e)
	}
	if len(overview.Rounds) != 1 || !overview.Rounds[0].AllExported || overview.Rounds[0].IssuedVersion != 1 {
		t.Fatalf("overview=%+v", overview)
	}
	signers, e := svc.DocProgress.ListChecklist(ctx, term, 202606)
	if e != nil {
		t.Fatal(e)
	}
	if len(signers) != 3 {
		t.Fatalf("signers=%+v", signers)
	}
	exec(`UPDATE ta_request_assignments SET state='dropped' WHERE ta_id=$1`, ta)
	historicalOverview, e := svc.DocProgress.GetOverview(ctx, term)
	if e != nil {
		t.Fatal(e)
	}
	if !historicalOverview.Rounds[0].AllExported || historicalOverview.Rounds[0].TotalCourses != 1 {
		t.Fatalf("issued progress changed after roster update: %+v", historicalOverview.Rounds[0])
	}
	if e = svc.DocProgress.ToggleSignature(ctx, actor, course, "ta", &ta, true, 202606); e != nil {
		t.Fatal(e)
	}
	if e = svc.DocProgress.SetStage(ctx, actor, term, 1, "TA signed", 202606); e != nil {
		t.Fatal(e)
	}
	if e = svc.DocProgress.SetStage(ctx, actor, term, 4, "skip signatures", 202606); e == nil {
		t.Fatal("finance stage accepted unsigned lecturer/certifier")
	}
	exec(`UPDATE ta_request_assignments SET state='active' WHERE ta_id=$1`, ta)

	// Adding somebody after issue must not add a signature to the old paper.
	exec(`UPDATE users SET first_name='renamed later' WHERE id=$1`, ta)
	again, e := svc.DocProgress.ListChecklist(ctx, term, 202606)
	if e != nil {
		t.Fatal(e)
	}
	for i := range signers {
		if signers[i].Responsible != again[i].Responsible {
			t.Fatal("issued signer roster changed")
		}
	}

	// Exercise a fresh month with no cached course ZIP, no work logs, and no
	// monthly submission rows. It must still pay the remaining 500-baht lump.
	exec(`INSERT INTO submission_periods(id,term_id,year_month,label,starts_on,due_date,is_closed) VALUES(gen_random_uuid(),$1,'2569-07','กรกฎาคม 2569','2026-07-01',CURRENT_DATE+1,false)`, term)
	julyURL := "/exports/terms/" + term.String() + "/monthly-payout.zip?months=2026-07"
	early, e := app.Test(httptest.NewRequest("POST", julyURL, nil), -1)
	if e != nil {
		t.Fatal(e)
	}
	early.Body.Close()
	if early.StatusCode != 400 {
		t.Fatalf("open month status=%d", early.StatusCode)
	}
	exec(`UPDATE submission_periods SET is_closed=true WHERE term_id=$1 AND year_month='2569-07'`, term)
	exec(`INSERT INTO pay_rates(effective_from,undergrad_regular,undergrad_special,graduate_regular,graduate_special_lumpsum,graduate_regular_hourly,grad_special_term_cap,term_months,daily_pay_cap_baht,ug_regular_daily_hour_cap,ug_special_daily_hour_cap,grad_regular_daily_hour_cap,ug_lecture_hours_per_credit,ug_lab_hours_per_credit,baseline_students_lecture,baseline_students_lab,ug_workload_rate_regular) VALUES('2020-01-01',50,60,300,1000,50,12000,2,100000,24,24,24,1,1,30,30,10)`)
	exec(`UPDATE teaching_courses SET num_students=30,num_students_special=30,num_students_regular=0 WHERE id=$1`, course)
	exec(`INSERT INTO teaching_lecturers(teaching_course_id,lecturer_id,is_primary) VALUES($1,$2,true)`, course, actor)
	exec(`INSERT INTO ta_profiles(user_id,status,current_round) VALUES($1,'approved',1)`, ta)
	pdfPath, _, e := store.Save("ta_documents", "creditor.pdf", bytes.NewReader([]byte("%PDF-1.4 test creditor form")))
	if e != nil {
		t.Fatal(e)
	}
	exec(`INSERT INTO ta_documents(id,user_id,kind,filename,mime,size_bytes,storage_key,status) VALUES(gen_random_uuid(),$1,'creditor_form','creditor.pdf','application/pdf',28,$2,'approved')`, ta, pdfPath)
	orderID := uuid.New()
	exec(`INSERT INTO appointment_orders(id,term_id,round_no,order_no,order_date,effective_date,ta_count) VALUES($1,$2,1,'TEST-1','2569-06-01','2569-06-01',1)`, orderID, term)
	exec(`INSERT INTO appointment_order_items(id,appointment_order_id,teaching_course_id,ta_id) VALUES(gen_random_uuid(),$1,$2,$3)`, orderID, course, ta)
	svc.Storage = monthlyFailingStore{Store: store}
	failed, e := app.Test(httptest.NewRequest("POST", julyURL, nil), -1)
	if e != nil {
		t.Fatal(e)
	}
	failedBody, _ := io.ReadAll(failed.Body)
	failed.Body.Close()
	if failed.StatusCode != 500 {
		t.Fatalf("storage-failure status=%d body=%s", failed.StatusCode, failedBody)
	}
	var julyLocked int
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM grad_lump_ledger WHERE teaching_course_id=$1 AND year_month='2026-07'`, course).Scan(&julyLocked); e != nil {
		t.Fatal(e)
	}
	if julyLocked != 0 {
		t.Fatal("storage failure froze money")
	}
	svc.Storage = store
	paidJuly, e := app.Test(httptest.NewRequest("POST", julyURL, nil), -1)
	if e != nil {
		t.Fatal(e)
	}
	defer paidJuly.Body.Close()
	julyBody, _ := io.ReadAll(paidJuly.Body)
	if paidJuly.StatusCode != 200 {
		t.Fatalf("fresh month status=%d body=%s", paidJuly.StatusCode, julyBody)
	}
	julyZip, e := zip.NewReader(bytes.NewReader(julyBody), int64(len(julyBody)))
	if e != nil {
		t.Fatal(e)
	}
	var manifest struct {
		Total float64 `json:"total_baht"`
		Month string  `json:"year_month"`
	}
	for _, entry := range julyZip.File {
		if entry.Name == "manifest.json" {
			r, e := entry.Open()
			if e != nil {
				t.Fatal(e)
			}
			e = json.NewDecoder(r).Decode(&manifest)
			r.Close()
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	if manifest.Total != 500 || manifest.Month != "2026-07" {
		t.Fatalf("monthly manifest=%+v", manifest)
	}
	var fullPaid float64
	if e = pool.QueryRow(ctx, `SELECT sum(baht)::float8 FROM grad_lump_ledger WHERE teaching_course_id=$1`, course).Scan(&fullPaid); e != nil {
		t.Fatal(e)
	}
	if fullPaid != 1000 {
		t.Fatalf("whole term paid=%.2f want1000", fullPaid)
	}

}

// Fault injection only affects the combined file. Prepared per-course files
// may exist, but no money or batch marker may commit when this save fails.
type monthlyFailingStore struct{ storage.Store }

func (s monthlyFailingStore) Save(kind, name string, r io.Reader) (string, int64, error) {
	if kind == "monthly_payout_batches" {
		return "", 0, errors.New("simulated combined archive storage failure")
	}
	return s.Store.Save(kind, name, r)
}
