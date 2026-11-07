package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type regSec struct{ no, open, enrolled int }

// The registrar as the tests see it: SC363001 and CP363205 as saved from
// reg.kku.ac.th on 03/10/2026, plus any extra codes a test adds.
var fakeRegCourses = map[string]struct {
	id   string
	secs []regSec
}{
	"SC363001": {"3613665151", []regSec{{1, 85, 73}, {2, 20, 7}}},
	"CP363205": {"3613670353", []regSec{{1, 51, 49}, {2, 45, 41}}},
}

// fakeReg serves the new guest API and counts data searches separately from
// token/metadata calls. Section 2 is special in these fixtures.
func fakeReg(t *testing.T, extra map[string][]regSec) (*httptest.Server, *int32, *sync.Map) {
	t.Helper()
	courses := map[string][]regSec{}
	for code, c := range fakeRegCourses {
		courses[code] = c.secs
	}
	for code, secs := range extra {
		courses[code] = secs
	}
	var hits int32
	kinds := &sync.Map{}
	count := func(k string) { n, _ := kinds.LoadOrStore(k, new(int32)); atomic.AddInt32(n.(*int32), 1) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if r.URL.Path == "/Validate/tokenservice" {
			enc.Encode(map[string]string{"token": "guest"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer guest" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/ComboLev/AllClassinfo" {
			enc.Encode([]map[string]any{{"comboid": 31, "comboshow": "ปริญญาตรี ภาคปกติ"}, {"comboid": 34, "comboshow": "ปริญญาตรี โครงการพิเศษ"}})
			return
		}
		p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(p) != 11 || p[0] != "Classinfo" || p[1] != "Classinfo" {
			http.NotFound(w, r)
			return
		}
		prefix, level := p[8], p[7]
		if len(prefix) < 8 {
			count("search")
		} else {
			count("list")
		}
		var codes []string
		for code := range courses {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		rows := []map[string]any{}
		for index, code := range codes {
			if !strings.HasPrefix(code, prefix) {
				continue
			}
			for _, sc := range courses[code] {
				special := sc.no == 2
				if level == "31" && special || level == "34" && !special {
					continue
				}
				year, _ := strconv.Atoi(p[4])
				sem, _ := strconv.Atoi(p[5])
				rows = append(rows, map[string]any{"classid": 1000 + index*100 + sc.no, "courseid": 9000 + index, "acadyear": year, "semester": sem, "coursecode": code, "section": strconv.Itoa(sc.no), "totalseat": sc.open, "enrollseat": sc.enrolled})
			}
		}
		start, _ := strconv.Atoi(p[2])
		limit, _ := strconv.Atoi(p[3])
		total := len(rows)
		begin := min(start-1, total)
		end := min(begin+limit, total)
		enc.Encode([]map[string]any{{"row": start, "limit": limit, "count": total, "classinfolist": rows[begin:end]}})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, kinds
}

func kindCount(m *sync.Map, k string) int32 {
	if n, ok := m.Load(k); ok {
		return atomic.LoadInt32(n.(*int32))
	}
	return 0
}

// regFixture: a course whose own code is SC363001, with our sections 1
// (regular) and 2 (special) — so its tracks need no detail page.
func regFixture(t *testing.T, alts string) *fixture {
	f := newFixture(t, fixtureOpts{NoRequest: true})
	f.exec(`UPDATE teaching_courses SET code = 'SC363001', alt_codes = `+alts+` WHERE id = $1`, f.CourseID)
	f.exec(`UPDATE sections SET sec_no = '01', track = 'regular' WHERE id = $1`, f.SectionID)
	f.exec(`INSERT INTO sections (teaching_course_id, sec_no, track) VALUES ($1, '02', 'special')`, f.CourseID)
	return f
}

func waitRegJob(t *testing.T, s *RegEnrolmentService, f *fixture) *RegEnrolmentJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j, err := s.Status(f.ctx, f.StaffID, f.TermID)
		if err != nil {
			t.Fatal(err)
		}
		if j != nil && j.Status != "running" {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fetch did not finish")
	return nil
}

func TestRegEnrolment_SumsByAPITrackAndCachesResults(t *testing.T) {
	f := regFixture(t, `ARRAY['CP363205']`)
	srv, hits, kinds := fakeReg(t, nil)
	s := NewRegEnrolmentService(f.Pool, srv.URL)
	s.reg.Gap, s.reg.Jitter = 0, 0

	if _, err := s.Start(f.ctx, f.LecturerID, f.TermID, RegScopeAll); err == nil {
		t.Fatal("a lecturer must not start a registrar fetch")
	}
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitRegJob(t, s, f)
	if j.Status != "done" || j.Done != 2 || j.Total != 2 {
		t.Fatalf("job = %+v", j)
	}
	got := map[string]RegEnrolmentRow{}
	for _, r := range j.Rows {
		got[r.Code] = r
	}
	if r := got["SC363001"]; r.Regular != 73 || r.Special != 7 || r.Error != "" {
		t.Errorf("SC363001 = %+v, want 73 regular / 7 special", r)
	}
	if r := got["CP363205"]; r.Regular != 49 || r.Special != 41 {
		t.Errorf("CP363205 = %+v, want 49 / 41", r)
	}

	if kindCount(kinds, "detail") != 0 {
		t.Fatal("new API must not request ASP detail pages")
	}
	first := atomic.LoadInt32(hits)
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(hits) != first {
		t.Fatal("fresh result fetched again")
	}
	old := time.Now().Add(-time.Hour)
	age := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.jobs[regJobKey{f.TermID, "new"}].FinishedAt = &old
		for k, v := range s.codes {
			v.at = old
			s.codes[k] = v
		}
	}
	age()
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	waitRegJob(t, s, f)
	if atomic.LoadInt32(hits) <= first {
		t.Fatal("expired results must refresh")
	}

	// Exported courses can no longer change and are not fetched.
	f.exec(`UPDATE teaching_courses SET exported_at = NOW() WHERE id = $1`, f.CourseID)
	age()
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err == nil {
		t.Error("a term with only exported courses must refuse to fetch")
	}
}

// Five or more codes sharing a prefix are read with one wildcard search
// instead of one search each.
func TestRegEnrolment_LargeGroupUsesOneWildcardSearch(t *testing.T) {
	f := regFixture(t, `ARRAY['CP363205','CP000001','CP000002','CP000003','CP000004']`)
	extra := map[string][]regSec{}
	for i := 1; i <= 4; i++ {
		code := fmt.Sprintf("CP00000%d", i)
		extra[code] = []regSec{{1, 30, 10 + i}}
		f.exec(`INSERT INTO reg_kku_section_tracks (academic_year, semester, code, sec_no, special)
		        SELECT academic_year, semester, $2, '1', false FROM academic_terms WHERE id = $1`, f.TermID, code)
	}
	srv, hits, kinds := fakeReg(t, extra)
	s := NewRegEnrolmentService(f.Pool, srv.URL)
	s.reg.Gap, s.reg.Jitter = 0, 0

	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	j := waitRegJob(t, s, f)
	for _, r := range j.Rows {
		if r.Error != "" {
			t.Errorf("%s: %s", r.Code, r.Error)
		}
		if r.Code == "CP000003" && r.Regular != 13 {
			t.Errorf("CP000003 = %+v, want 13 regular from the wildcard page", r)
		}
	}
	// Three CP prefix searches (all, regular, special), three for SC,
	// plus one token and one level metadata request; no per-code CP queries.
	if kindCount(kinds, "search") != 3 || kindCount(kinds, "list") != 3 || atomic.LoadInt32(hits) != 8 {
		t.Fatalf("requests: %d prefix, %d exact, total %d", kindCount(kinds, "search"), kindCount(kinds, "list"), atomic.LoadInt32(hits))
	}
}

// "Only courses that asked for a TA" fetches just those, and a later "all"
// reuses their fresh answers instead of asking the registrar again.
func TestRegEnrolment_RequestedScopeThenAllReusesAnswers(t *testing.T) {
	f := regFixture(t, `'{}'`)
	srv, hits, _ := fakeReg(t, nil)
	s := NewRegEnrolmentService(f.Pool, srv.URL)
	s.reg.Gap, s.reg.Jitter = 0, 0

	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeRequested); err == nil {
		t.Fatal("no course has asked for a TA: the requested scope must say so, not fetch")
	}
	if *hits != 0 {
		t.Fatalf("requests sent for an empty scope: %d", *hits)
	}

	f.exec(`INSERT INTO ta_requests (teaching_course_id, lecturer_id, reimburse_scope, status, submitted_at)
	        VALUES ($1, $2, 'both', 'submitted', NOW())`, f.CourseID, f.LecturerID)
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeRequested); err != nil {
		t.Fatalf("requested: %v", err)
	}
	if j := waitRegJob(t, s, f); j.Scope != RegScopeRequested || len(j.Rows) != 1 || j.Rows[0].Regular != 73 {
		t.Fatalf("requested job = %+v", j)
	}
	first := *hits

	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatalf("all: %v", err)
	}
	j := waitRegJob(t, s, f)
	if j.Scope != RegScopeAll || j.Reused != 1 || *hits != first {
		t.Fatalf("all after requested: reused %d, %d new requests; want the fresh answer reused", j.Reused, *hits-first)
	}
}

// Stop ends the fetch at once, keeps what was fetched, and sends nothing more;
// starting again only fetches what is still missing.
func TestRegEnrolment_StopKeepsPartialResult(t *testing.T) {
	f := regFixture(t, `ARRAY['CP363205','XX000001','XX000002','XX000003']`)
	srv, hits, _ := fakeReg(t, nil)
	s := NewRegEnrolmentService(f.Pool, srv.URL)
	s.reg.Gap, s.reg.Jitter = 200*time.Millisecond, 0

	if _, err := s.Stop(f.ctx, f.StaffID, f.TermID); err == nil {
		t.Fatal("stopping with nothing running must say so")
	}
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		j, _ := s.Status(f.ctx, f.StaffID, f.TermID)
		if j.Done >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, err := s.Stop(f.ctx, f.StaffID, f.TermID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "stopped" || len(j.Rows) == 0 || len(j.Rows) >= j.Total {
		t.Fatalf("after stop: %+v", j)
	}
	for _, r := range j.Rows {
		if r.Error == regErrNetwork {
			t.Fatalf("the cancelled request was reported as a registrar failure: %+v", r)
		}
	}
	after := atomic.LoadInt32(hits)
	time.Sleep(600 * time.Millisecond)
	if n := atomic.LoadInt32(hits); n != after {
		t.Fatalf("%d requests sent after stop", n-after)
	}

	kept := len(j.Rows)
	s.reg.Gap = 0
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	j2 := waitRegJob(t, s, f)
	if j2.Status != "done" || j2.Reused != kept || len(j2.Rows) != j2.Total {
		t.Fatalf("restart: %+v (want the %d stopped-run answers reused)", j2, kept)
	}
}

func TestRegEnrolment_IncompleteMergedCourseIsNotUsable(t *testing.T) {
	f := regFixture(t, `ARRAY['XX000001']`)
	srv, _, _ := fakeReg(t, nil)
	s := NewRegEnrolmentService(f.Pool, srv.URL)
	s.reg.Gap, s.reg.Jitter = 0, 0
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	j := waitRegJob(t, s, f)
	for _, row := range j.Rows {
		if row.Error == "" {
			t.Fatalf("partial merged total must not be usable: %+v", row)
		}
	}
}

func TestRegEnrolment_CacheDoesNotCrossAcademicYear(t *testing.T) {
	f := regFixture(t, `'{}'`)
	srv, hits, _ := fakeReg(t, nil)
	s := NewRegEnrolmentService(f.Pool, srv.URL)
	s.reg.Gap, s.reg.Jitter = 0, 0
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	first := waitRegJob(t, s, f)
	before := atomic.LoadInt32(hits)
	f.exec(`UPDATE academic_terms SET academic_year = academic_year + 1 WHERE id = $1`, f.TermID)
	if stale, err := s.Status(f.ctx, f.StaffID, f.TermID); err != nil || stale != nil {
		t.Fatalf("old completed job would populate the edited term: job=%+v err=%v", stale, err)
	}
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	second := waitRegJob(t, s, f)
	if second.Year != first.Year+1 || second.Reused != 0 || atomic.LoadInt32(hits) <= before {
		t.Fatalf("stale term cache used: %+v", second)
	}
}

func TestRegEnrolment_FailedPrefixDoesNotFanOut(t *testing.T) {
	f := regFixture(t, `ARRAY['CP000001','CP000002','CP000003','CP000004','CP000005']`)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/Validate/tokenservice" {
			json.NewEncoder(w).Encode(map[string]string{"token": "guest"})
			return
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(503)
	}))
	defer srv.Close()
	s := NewRegEnrolmentService(f.Pool, srv.URL)
	s.reg.Gap, s.reg.Jitter = 0, 0
	s.reg.MaxBackoff = time.Millisecond
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	j := waitRegJob(t, s, f)
	if j.Status != "failed" || hits.Load() != 2 {
		t.Fatalf("job=%+v requests=%d", j, hits.Load())
	}
}
