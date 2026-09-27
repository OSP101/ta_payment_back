package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ta-payment-back/internal/audit"
)

// TDBM is the source of truth for holidays and makeups (faculty decision
// 27/09/2026), so what it brings in must follow the same sitting rules as a
// holiday or makeup entered by hand.

func (f *fixture) tdbmWith(holidays []tdbmHolidayRow) *TDBMService {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(holidays)
	}))
	f.t.Cleanup(srv.Close)
	return &TDBMService{pool: f.Pool, aud: audit.New(f.Pool), apiBase: srv.URL}
}

func TestTDBMSyncHolidays_ClearsDraftsAndSkipsSentDays(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	mon := firstMonday()
	next := monthStart().AddDate(0, 0, firstMondayOffset()+7).Format("2006-01-02")
	draft := f.insertLog(mon, "lab", "draft", "13:00", "16:00")
	f.insertLog(next, "lab", "approved", "13:00", "16:00")

	res := f.tdbmWith([]tdbmHolidayRow{
		{HolidayID: 90001, HDate: mon, Title: "วันหยุดจาก TDBM", HType: "E"},
		{HolidayID: 90002, HDate: next, Title: "วันหยุดทับคาบที่อนุมัติแล้ว", HType: "E"},
	}).SyncHolidays(f.ctx, "test", f.AcademicYear, 1)

	var n int
	if err := f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM work_logs WHERE id = $1`, draft).Scan(&n); err != nil || n != 0 {
		t.Fatalf("draft of the class a TDBM holiday cancelled survived: n=%d err=%v", n, err)
	}
	var have bool
	if err := f.Pool.QueryRow(f.ctx,
		`SELECT EXISTS (SELECT 1 FROM public_holidays WHERE tdbm_holiday_id = 90002)`).Scan(&have); err != nil {
		t.Fatal(err)
	}
	if have {
		t.Fatal("a TDBM holiday landed on a day with an approved class sitting")
	}
	if !strings.Contains(res.Error, next) {
		t.Fatalf("sync result should name the skipped day %s, got %q", next, res.Error)
	}
}

func TestTDBMAutoFillMakeup_FollowsSittingRules(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	s := &TDBMService{pool: f.Pool, aud: audit.New(f.Pool)}
	mon := firstMonday()
	next := monthStart().AddDate(0, 0, firstMondayOffset()+7).Format("2006-01-02")

	// The original sitting was taught (sent hours): no makeup on top of it.
	f.insertLog(mon, "lab", "submitted", "13:00", "16:00")
	n, err := s.applyOneMakeup(f.ctx, f.SectionID,
		unresolvedPeriod{SectionID: f.SectionID, HolidayDate: mon, Kind: "lab"},
		tdbmCandidate{ExtraClassID: 1, SectionID: f.SectionID, ClassDate: day(27), StartTime: "13:00", EndTime: "16:00"})
	if err != nil || n != 0 {
		t.Fatalf("makeup filed over a taught sitting: n=%d err=%v", n, err)
	}

	// Only a draft on the original sitting: makeup filed, draft removed.
	draft := f.insertLog(next, "lab", "draft", "13:00", "16:00")
	n, err = s.applyOneMakeup(f.ctx, f.SectionID,
		unresolvedPeriod{SectionID: f.SectionID, HolidayDate: next, Kind: "lab"},
		tdbmCandidate{ExtraClassID: 2, SectionID: f.SectionID, ClassDate: day(28), StartTime: "13:00", EndTime: "16:00"})
	if err != nil || n != 1 {
		t.Fatalf("makeup not filed: n=%d err=%v", n, err)
	}
	var left int
	if err := f.Pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM work_logs WHERE id = $1`, draft).Scan(&left); err != nil || left != 0 {
		t.Fatalf("draft of the replaced sitting survived: n=%d err=%v", left, err)
	}
}
