package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/timeutil"
)

// Batch 4: the audit trail's before-images are taken under a lock, and that
// lock must not introduce deadlocks.

func isDeadlock(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "40P01"
}

// A course-level edit (locks the course, then writes its sections) racing a
// section edit (used to lock the section, then write the course) is an AB-BA
// pair. With FOR UPDATE on before-images it would deadlock unless every path
// locks course → section. Run them against each other repeatedly.
func TestCourseAndSectionEdits_DoNotDeadlock(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := newTeachingSvc(t, f)
	ctx := context.Background()

	const rounds = 20
	for r := 0; r < rounds; r++ {
		credits := 3 + r%2
		curriculum := []string{"CS", "IT"}[r%2]
		students := 30 + r
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errs[0] = svc.UpdateCourseInfo(ctx, f.StaffID, f.CourseID,
				UpdateCourseInfoInput{Credits: &credits, Curriculum: &curriculum})
		}()
		go func() {
			defer wg.Done()
			<-start
			errs[1] = svc.UpdateSection(ctx, f.StaffID, f.CourseID, f.SectionID,
				UpdateSectionInput{NumStudents: &students})
		}()
		close(start)
		wg.Wait()
		for i, err := range errs {
			if isDeadlock(err) {
				t.Fatalf("round %d: deadlock between course and section edit (writer %d): %v", r, i, err)
			}
			if err != nil {
				t.Fatalf("round %d writer %d: %v", r, i, err)
			}
		}
	}
}

// Concurrent edits of ONE work log: each audit row's before-image must equal the
// previous row's after-image. Without the lock a writer could record a "before"
// that another writer had already replaced.
func TestAuditBeforeImages_ChainUnderConcurrentEdits(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	id := f.mustUpsert(f.entry(day(10), "09:00", "10:00", 1))
	ctx := context.Background()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, end := range []string{"11:00", "12:00", "13:00", "11:30", "12:30", "10:30"} {
		wg.Add(1)
		go func(i int, end string) {
			defer wg.Done()
			<-start
			w := f.entry(day(10), "09:00", end, 0)
			w.ID = id
			// hours follow the time span; a rejected cap is fine, a wrong chain is not
			w.Hours = map[string]float64{"10:30": 1.5, "11:00": 2, "11:30": 2.5, "12:00": 3, "12:30": 3.5, "13:00": 4}[end]
			_, _ = f.Svc.StaffUpsert(ctx, f.StaffID, true, w, nil)
		}(i, end)
	}
	close(start)
	wg.Wait()

	rows, err := f.Pool.Query(ctx, `
		SELECT before, after FROM audit_logs
		WHERE entity = 'work_log' AND entity_id = $1 AND action = 'worklog.staff_edit'
		ORDER BY at, id`, id.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var prevAfterHours any
	n := 0
	for rows.Next() {
		var b, a []byte
		if err := rows.Scan(&b, &a); err != nil {
			t.Fatal(err)
		}
		var before, after map[string]any
		_ = json.Unmarshal(b, &before)
		_ = json.Unmarshal(a, &after)
		if n > 0 && before != nil && prevAfterHours != nil && before["hours"] != nil &&
			!jsonEqual(before["hours"], prevAfterHours) {
			t.Fatalf("edit %d recorded before.hours=%v but the previous edit left %v — the trail skipped a change",
				n, before["hours"], prevAfterHours)
		}
		if after != nil && after["hours"] != nil {
			prevAfterHours = after["hours"]
		}
		n++
	}
	if n < 2 {
		t.Skipf("only %d edits committed; nothing to chain", n)
	}
}

// The TA's own timetable decides which hours are payable, so saving it is on the
// audit trail with what it replaced.
func TestReplaceClasses_IsAudited(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	svc := &WorkloadService{pool: f.Pool, aud: audit.New(f.Pool)}
	if err := svc.ReplaceClasses(f.ctx, f.TAID, f.TermID, []ClassBlock{{
		CourseCode: "ZZ111", Kind: "lecture", DayOfWeek: 2, StartTime: "13:00", EndTime: "15:00",
	}}); err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err := f.Pool.QueryRow(f.ctx, `
		SELECT before::text, after::text FROM audit_logs
		WHERE action = 'ta_class_schedule.replace' AND actor_id = $1`, f.TAID).Scan(&before, &after); err != nil {
		t.Fatalf("no audit row for a timetable save: %v", err)
	}
	if !strings.Contains(before, "ZZ000") { // the fixture's own Sunday class
		t.Errorf("before-image should hold the replaced timetable, got %s", before)
	}
	if !strings.Contains(after, "ZZ111") {
		t.Errorf("after-image should hold the new timetable, got %s", after)
	}
}

// ข้อ 15, approval half: a class added to the TA's timetable AFTER the hours were
// submitted must stop those hours from being approved.
func TestApprove_RefusesRowsThatNowClashWithOwnClass(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.mustUpsert(f.entry(day(10), "09:00", "11:00", 2))
	if err := f.Svc.Submit(f.ctx, f.TAID, f.AssignmentID); err != nil {
		t.Fatal(err)
	}
	d, err := timeutil.ParseDate(day(10))
	if err != nil {
		t.Fatal(err)
	}
	wd := int(d.Weekday())
	svc := &WorkloadService{pool: f.Pool, aud: audit.New(f.Pool)}
	if err := svc.ReplaceClasses(f.ctx, f.TAID, f.TermID, []ClassBlock{
		{CourseCode: "ZZ000", Kind: "lecture", DayOfWeek: 0, StartTime: "07:00", EndTime: "08:00"},
		{CourseCode: "CLASH1", Kind: "lecture", DayOfWeek: wd, StartTime: "09:00", EndTime: "12:00"},
	}); err != nil {
		t.Fatal(err)
	}
	err = f.Svc.Approve(f.ctx, f.LecturerID, f.AssignmentID, "", false)
	if userErrStatus(err) != 409 || !strings.Contains(err.Error(), "ตารางเรียน") {
		t.Fatalf("approval must refuse a row that now clashes with the TA's class, got %v", err)
	}
}
