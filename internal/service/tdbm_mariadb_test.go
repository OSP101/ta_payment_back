package service

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"

	"ta-payment-back/internal/config"
)

func tdbmPtr[T any](v T) *T { return &v }

func (f *fixture) mariaEntry(id int, original, date, kind string) tdbmExtraTeachingRow {
	var code, sec string
	if err := f.Pool.QueryRow(f.ctx, `SELECT tc.code,s.sec_no FROM sections s JOIN teaching_courses tc ON tc.id=s.teaching_course_id WHERE s.id=$1`, f.SectionID).Scan(&code, &sec); err != nil {
		f.t.Fatal(err)
	}
	return tdbmExtraTeachingRow{ExtraClassID: id, ClassDate: date, StartTime: "09:00:00", EndTime: "12:00:00", Duration: 180,
		CourseCode: &code, Section: &sec, SemesterType: tdbmPtr("ภาคปกติ"), OptStatus: "D", ClassID: tdbmPtr(100), TeachingID: &id,
		OriginalDate: &original, OriginalKind: &kind, OriginalStart: tdbmPtr("09:00:00"), OriginalEnd: tdbmPtr("12:00:00")}
}

func TestTDBMMariaDBMirror_ExactOriginalAndChanges(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	// Two originals and two replacement days, with the earlier original moved
	// to the later day. Exact IDs must work without a holiday calendar or guesses.
	a := f.mariaEntry(980001, firstMonday(), day(28), "lecture")
	b := f.mariaEntry(980002, day(14), day(27), "lab")
	b.OptStatus = "A"
	p := f.mariaEntry(980003, day(15), day(26), "lecture")
	p.OptStatus = "P"
	r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a, b, p})
	if r.Error != "" || f.makeupCount() != 2 {
		t.Fatalf("initial: %+v count=%d", r, f.makeupCount())
	}
	var original, date string
	if err := f.Pool.QueryRow(f.ctx, `SELECT m.original_date::text,m.makeup_date::text FROM makeup_schedules m JOIN tdbm_extra_teachings t ON t.applied_makeup_id=m.id WHERE extra_class_id=$1`, a.ExtraClassID).Scan(&original, &date); err != nil || original != *a.OriginalDate || date != a.ClassDate {
		t.Fatalf("wrong original: %s/%s err=%v", original, date, err)
	}
	a.ClassDate = day(26)
	a.StartTime = "10:00:00"
	a.EndTime = "11:00:00"
	a.Duration = 60
	r = s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a, b, p})
	if r.Error != "" || f.makeupCount() != 2 {
		t.Fatalf("reschedule: %+v", r)
	}
	if err := f.Pool.QueryRow(f.ctx, `SELECT m.makeup_date::text FROM makeup_schedules m JOIN tdbm_extra_teachings t ON t.applied_makeup_id=m.id WHERE extra_class_id=$1`, a.ExtraClassID).Scan(&date); err != nil || date != a.ClassDate {
		t.Fatalf("stale makeup: %s err=%v", date, err)
	}
	// Explicit deletion and a hard disappearance both remove applied makeups.
	a.OptStatus = "X"
	r = s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a, p})
	if r.Error != "" || f.makeupCount() != 0 {
		t.Fatalf("deletion: %+v count=%d", r, f.makeupCount())
	}
	var missing bool
	if err := f.Pool.QueryRow(f.ctx, `SELECT source_missing FROM tdbm_extra_teachings WHERE extra_class_id=$1`, b.ExtraClassID).Scan(&missing); err != nil || !missing {
		t.Fatalf("missing=%v err=%v", missing, err)
	}
	// A complete empty snapshot must reconcile too, rather than early-return.
	r = s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, nil)
	if r.Error != "" {
		t.Fatalf("empty: %+v", r)
	}
}

func TestTDBMMariaDBMirror_ProtectsSentHoursAndRetries(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	a := f.mariaEntry(980001, firstMonday(), day(28), "lecture")
	if r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a}); r.Error != "" {
		t.Fatal(r.Error)
	}
	log := f.insertLog(day(28), "lecture", "submitted", "09:00", "12:00")
	a.OptStatus = "C"
	r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a})
	if r.Error != "" || r.Warning == "" || f.makeupCount() != 1 {
		t.Fatalf("sent evidence modified: %+v count=%d", r, f.makeupCount())
	}
	rows, err := s.ListExtraTeachings(f.ctx, f.AcademicYear, 1)
	if err != nil || len(rows) != 1 || rows[0].ReconciliationError == nil || len(rows[0].MatchOptions) != 0 {
		t.Fatalf("conflict not visible: %+v err=%v", rows, err)
	}
	f.exec(`DELETE FROM work_logs WHERE id=$1`, log)
	r = s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a})
	if r.Error != "" || f.makeupCount() != 0 {
		t.Fatalf("retry: %+v count=%d", r, f.makeupCount())
	}
}

func TestTDBMMariaDBMirror_AliasesAndConflictingReplacements(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	a := f.mariaEntry(980001, firstMonday(), day(28), "lecture")
	b := a
	b.ExtraClassID++
	if r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a, b}); r.Error != "" || f.makeupCount() != 1 {
		t.Fatalf("aliases %+v count=%d", r, f.makeupCount())
	}
	// Cancelling one alias cannot cancel an unchanged eligible owner.
	a.OptStatus = "C"
	if r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a, b}); r.Error != "" || f.makeupCount() != 1 {
		t.Fatalf("cancel alias %+v", r)
	}
	a.OptStatus = "D"
	a.ClassDate = day(26)
	if r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a, b}); r.Error != "" || r.Warning == "" || f.makeupCount() != 1 {
		t.Fatalf("divergent aliases %+v", r)
	}
}

func TestTDBMMariaDBMirror_FailedReadPreservesMirror(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	a := f.mariaEntry(980001, firstMonday(), day(28), "lecture")
	s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a})
	s.source = tdbmSourceFunc(func(context.Context, int, int) (tdbmSnapshot, error) {
		return tdbmSnapshot{}, errors.New("upstream unavailable")
	})
	if r := s.SyncExtraTeachings(f.ctx, "manual", f.AcademicYear, 1); r.Error == "" || f.makeupCount() != 1 {
		t.Fatalf("failed read %+v", r)
	}
	var missing bool
	if err := f.Pool.QueryRow(f.ctx, `SELECT source_missing FROM tdbm_extra_teachings WHERE extra_class_id=$1`, a.ExtraClassID).Scan(&missing); err != nil || missing {
		t.Fatalf("failed read tombstoned mirror: %v %v", missing, err)
	}
}

func TestTDBMMariaDBMirror_ExportLockIsPreserved(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	a := f.mariaEntry(980001, firstMonday(), day(28), "lecture")
	if r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a}); r.Error != "" {
		t.Fatal(r.Error)
	}
	f.addSubmissionPeriod(firstMonday()[5:7], day(28), "exported", false)
	a.OptStatus = "X"
	r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a})
	if r.Error != "" || r.Warning == "" || f.makeupCount() != 1 {
		t.Fatalf("export evidence changed: %+v", r)
	}
}

func TestTDBMMariaDBMirror_UnopenedCourseStaysUnmatched(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	a := f.mariaEntry(980001, firstMonday(), day(28), "lecture")
	a.CourseCode = tdbmPtr("UNKNOWN-COURSE")
	if r := s.syncExtraRows(f.ctx, "manual", f.AcademicYear, 1, []tdbmExtraTeachingRow{a}); r.Error != "" || f.makeupCount() != 0 {
		t.Fatalf("unopened course: %+v", r)
	}
}

func TestTDBMMariaDBSyncAll_OneSnapshotPerUsedTermAndSerialization(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	f.exec(`UPDATE academic_terms SET is_active=FALSE`)
	f.exec(`INSERT INTO academic_terms(id,academic_year,semester,is_active) VALUES(gen_random_uuid(),2570,1,TRUE)`)
	calls := map[int]int{}
	s.source = tdbmSourceFunc(func(_ context.Context, y, sem int) (tdbmSnapshot, error) { calls[y]++; return tdbmSnapshot{}, nil })
	results, err := s.SyncAll(f.ctx, "manual")
	if err != nil || len(results) != 4 || calls[2569] != 1 || calls[2570] != 1 {
		t.Fatalf("snapshots=%v results=%d err=%v", calls, len(results), err)
	}
	guard, err := f.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Rollback(f.ctx)
	if _, err = guard.Exec(f.ctx, `SELECT pg_advisory_xact_lock(736264219)`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SyncAll(f.ctx, "manual"); err == nil {
		t.Fatal("concurrent sync entered while lock was held")
	}
}

func TestTDBMMariaDBHolidays_RemovalsAndManualPreservation(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := f.reviewService()
	f.exec(`INSERT INTO public_holidays(holiday_date,name_th,source,tdbm_holiday_id) VALUES($1::date,'upstream','tdbm',987123)`, day(20))
	f.reviewHoliday(day(21))
	if err := s.removeAbsentHolidays(f.ctx, day(1), day(28), []int{}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM public_holidays`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("manual holiday removed: count=%d err=%v", count, err)
	}
}

// Explicit opt-in only: validates the actual read-only connector without
// writing to either the source database or the application database.
func TestTDBMMariaDBLiveSnapshot(t *testing.T) {
	if os.Getenv("TDBM_TEST_LIVE") != "1" {
		t.Skip("set TDBM_TEST_LIVE=1 to inspect the connected source")
	}
	port, _ := strconv.Atoi(os.Getenv("TDBM_DB_PORT"))
	if port == 0 {
		port = 3306
	}
	s := tdbmMariaDBSource{cfg: config.Config{TDBMDBHost: os.Getenv("TDBM_DB_HOST"), TDBMDBPort: port, TDBMDBName: os.Getenv("TDBM_DB_NAME"), TDBMDBUser: os.Getenv("TDBM_DB_USER"), TDBMDBPassword: os.Getenv("TDBM_DB_PASSWORD"), TDBMDBTLS: "false"}}
	snapshot, err := s.Snapshot(context.Background(), 2569, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Extra) == 0 || snapshot.StartDate == "" {
		t.Fatal("unexpected empty term")
	}
	eligible := 0
	for _, r := range snapshot.Extra {
		if tdbmMakeupEligible(r.OptStatus) {
			eligible++
			if r.TeachingID == nil || r.OriginalDate == nil || r.OriginalKind == nil || r.Duration <= 0 {
				t.Fatal("incomplete eligible original sitting")
			}
		}
	}
	t.Logf("read-only snapshot: total=%d eligible=%d holidays=%d", len(snapshot.Extra), eligible, len(snapshot.Holidays))
	if os.Getenv("TEST_DATABASE_URL") != "" {
		// Mirror real source rows into a throwaway local database only. Open one
		// matching course so the connector-to-reconciliation path is exercised.
		f := newFixture(t, fixtureOpts{TermStart: snapshot.StartDate, TermEnd: snapshot.EndDate, NoRequest: true})
		var selected *tdbmExtraTeachingRow
		for i := range snapshot.Extra {
			r := &snapshot.Extra[i]
			if tdbmMakeupEligible(r.OptStatus) && r.CourseCode != nil && r.Section != nil {
				selected = r
				break
			}
		}
		if selected == nil {
			t.Fatal("no usable live source sitting")
		}
		f.exec(`UPDATE academic_terms SET semester=2 WHERE id=$1`, f.TermID)
		f.exec(`UPDATE teaching_courses SET code=$1 WHERE id=$2`, selected.CourseCode, f.CourseID)
		track := "regular"
		if selected.SemesterType != nil && *selected.SemesterType == "ภาคพิเศษ" {
			track = "special"
		}
		f.exec(`UPDATE sections SET sec_no=$1,track=$2::section_track WHERE id=$3`, selected.Section, track, f.SectionID)
		result := f.reviewService().syncExtraRows(f.ctx, "manual", 2569, 2, snapshot.Extra)
		var count int
		if err = f.Pool.QueryRow(f.ctx, `SELECT count(*) FROM tdbm_extra_teachings`).Scan(&count); err != nil || count != len(snapshot.Extra) {
			t.Fatalf("live mirror count=%d err=%v", count, err)
		}
		if result.Error != "" && result.Error != tdbmReconcileConflict {
			t.Fatal("live mirror failed")
		}
		if f.makeupCount() == 0 {
			t.Fatal("live originals did not produce any local makeups")
		}
		t.Logf("isolated live mirror: staged=%d applied=%d", count, f.makeupCount())
	}
}
