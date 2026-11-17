package service

import (
	"errors"
	"testing"

	"ta-payment-back/internal/config"
)

func accountBatchInput() []TAAccountInput {
	return []TAAccountInput{{StudentID: "693380999-1", Email: "batch-synthetic@example.invalid", Title: "นาย", FirstName: "ทดสอบ", LastName: "นำเข้า", StudyLevel: "undergrad"}}
}
func TestTAAccountBatchValidation(t *testing.T) {
	good := accountBatchInput()
	if err := validateTAAccountInputs(good); err != nil {
		t.Fatal(err)
	}
	if err := validateTAAccountInputs(append(good, good[0])); err == nil {
		t.Fatal("accepted duplicate")
	}
	good[0].StudyLevel = ""
	if err := validateTAAccountInputs(good); err == nil {
		t.Fatal("accepted missing level")
	}
}
func TestTAAccountBatchAtomicCreateAndNoRequestsOrMail(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := NewTAAccountBatch(f.Pool, config.Config{})
	var beforeUsers, beforeRequests, beforeNotices int
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM users`).Scan(&beforeUsers)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM ta_requests`).Scan(&beforeRequests)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM notifications`).Scan(&beforeNotices)
	input := accountBatchInput()
	preview, err := svc.Import(f.ctx, f.StaffID, "test", input, false, nil)
	if err != nil || preview[0].Status != "ready" {
		t.Fatalf("preview: %v", err)
	}
	_, err = svc.Import(f.ctx, f.StaffID, "test", input, true, func([]TAAccountRecord) error { return errors.New("disk full") })
	if err == nil {
		t.Fatal("checkpoint failure accepted")
	}
	var users int
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM users`).Scan(&users)
	if users != beforeUsers {
		t.Fatal("checkpoint failure committed account")
	}
	var staged []TAAccountRecord
	records, err := svc.Import(f.ctx, f.StaffID, "test", input, true, func(r []TAAccountRecord) error { staged = r; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 1 || records[0].Status != "pending" || len(records[0].Password) < 18 {
		t.Fatal("missing pending credential")
	}
	if err = svc.ValidateCredential(f.ctx, records[0]); err != nil {
		t.Fatal(err)
	}
	var mustChange, complete bool
	var sid string
	var role, enrollment, requests, notices int
	f.Pool.QueryRow(f.ctx, `SELECT must_change_password,profile_completed,student_id FROM users WHERE id=$1`, records[0].UserID).Scan(&mustChange, &complete, &sid)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM user_roles WHERE user_id=$1 AND role='ta'`, records[0].UserID).Scan(&role)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM ta_enrollments WHERE user_id=$1 AND student_id=$2 AND ended_at IS NULL`, records[0].UserID, input[0].StudentID).Scan(&enrollment)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM ta_requests`).Scan(&requests)
	f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM notifications`).Scan(&notices)
	if !mustChange || complete || sid != input[0].StudentID || role != 1 || enrollment != 1 || requests != beforeRequests || notices != beforeNotices {
		t.Fatal("wrong account state or unrelated side effects")
	}
	again, err := svc.Import(f.ctx, f.StaffID, "again", input, true, func(r []TAAccountRecord) error { return nil })
	if err != nil || again[0].Status != "existing" || again[0].Password != "" {
		t.Fatalf("rerun: %v", err)
	}
	conflict := accountBatchInput()
	conflict[0].StudentID = "693380999-2"
	cl, err := svc.Import(f.ctx, f.StaffID, "conflict", conflict, false, nil)
	if err != nil || cl[0].Status != "conflict" {
		t.Fatalf("email conflict: %v", err)
	}
	f.exec(`UPDATE users SET must_change_password=FALSE WHERE id=$1`, records[0].UserID)
	if err = svc.ValidateCredential(f.ctx, records[0]); err == nil {
		t.Fatal("accepted used temporary password")
	}
}

func TestTAAccountBatchStudentProgramInactiveStateAndPasswordPreparation(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := NewTAAccountBatch(f.Pool, config.Config{})
	input := accountBatchInput()
	input[0].Department = "Computer Science (SC-CS)"
	inactive := input[0]
	inactive.StudentID = "693380998-2"
	inactive.Email = "inactive-synthetic@example.invalid"
	active := false
	inactive.IsActive = &active
	input = append(input, inactive)
	// Another connection can update users while bcrypt is prepared; the import
	// must not hold its users write lock during that CPU-heavy phase.
	called := false
	svc.Progress = func(done, total int) {
		if done == 0 {
			called = true
			conn, err := f.Pool.Acquire(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			if _, err := conn.Exec(f.ctx, `SET lock_timeout='500ms'`); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(f.ctx, `UPDATE users SET updated_at=updated_at WHERE id=$1`, f.StaffID); err != nil {
				t.Fatalf("password preparation held users write lock: %v", err)
			}
		}
	}
	result, err := svc.Import(f.ctx, f.StaffID, "all-students", input, true, func([]TAAccountRecord) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !called || len(result) != 2 || result[0].Status != "pending" || result[1].Status != "created_inactive" || result[1].Password != "" {
		t.Fatal("wrong credential/active state")
	}
	for i, r := range result {
		var department string
		var isActive bool
		var override *string
		if err = f.Pool.QueryRow(f.ctx, `SELECT department,is_active,ta_seniority_override FROM users WHERE id=$1`, r.UserID).Scan(&department, &isActive, &override); err != nil {
			t.Fatal(err)
		}
		if department != input[i].Department || isActive != input[i].active() || override != nil {
			t.Fatal("student program/state lost, or all students marked old")
		}
	}
	if err = svc.ValidateCredential(f.ctx, result[1]); err == nil {
		t.Fatal("inactive student can receive credentials")
	}
	again, err := svc.Import(f.ctx, f.StaffID, "all-students", input, false, nil)
	if err != nil || again[0].Status != "existing" || again[1].Status != "existing" {
		t.Fatalf("repeat preview: %v", err)
	}
}
