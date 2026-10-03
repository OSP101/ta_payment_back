package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
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

func regListRow(code, id string, sc regSec) string {
	return fmt.Sprintf(`<TR VALIGN=TOP Class=NormalDetail><TD WIDTH=30></TD><TD BGCOLOR=#F0F0F5>&nbsp;<A HREF=class_info_2.asp?backto=HOME&option=0&courseid=%s&acadyear=2569&semester=1&avs5931437=1>%s</A>&nbsp;</TD><TD BGCOLOR=#F0F0F5>&nbsp;Name</TD><TD NOWRAP BGCOLOR=#F0F0F5>&nbsp;3 (2-2-5)</TD><TD ALIGN=RIGHT BGCOLOR=#F0F0F5>%d&nbsp;</TD><TD ALIGN=RIGHT BGCOLOR=#F0F0F5>%d&nbsp;</TD><TD ALIGN=RIGHT BGCOLOR=#F0F0F5>%d&nbsp;</TD><TD ALIGN=RIGHT BGCOLOR=#F0F0F5>%d&nbsp;</TD><TD BGCOLOR=#F0F0F5>&nbsp;W</TD><TD></TD></TR>`,
		id, code, sc.no, sc.open, sc.enrolled, sc.open-sc.enrolled)
}

// fakeReg serves search and detail pages in TIS-620 like the real site and
// counts requests by kind ("list", "search" for a wildcard, "detail").
func fakeReg(t *testing.T, extra map[string][]regSec) (*httptest.Server, *int32, *sync.Map) {
	t.Helper()
	read := func(name string) string {
		b, err := os.ReadFile("../regkku/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	detail := map[string]string{
		"3613665151": read("class_info_2_SC363001.html"),
		"3613670353": read("class_info_2_CP363205.html"),
	}
	courses := map[string]struct {
		id   string
		secs []regSec
	}{}
	for k, v := range fakeRegCourses {
		courses[k] = v
	}
	for k, v := range extra {
		courses[k] = struct {
			id   string
			secs []regSec
		}{"9" + k[len(k)-6:], v}
	}
	enc := func(s string) []byte { b, _ := charmap.Windows874.NewEncoder().String(s); return []byte(b) }
	var hits int32
	kinds := &sync.Map{}
	count := func(k string) {
		n, _ := kinds.LoadOrStore(k, new(int32))
		atomic.AddInt32(n.(*int32), 1)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		q := r.URL.Query()
		switch {
		case strings.HasSuffix(r.URL.Path, "class_info_1.asp"):
			want := q.Get("coursecode")
			prefix, wild := strings.CutSuffix(want, "*")
			if wild {
				count("search")
			} else {
				count("list")
			}
			var b strings.Builder
			b.WriteString("<html>รหัสวิชา <TABLE>")
			for code, c := range courses {
				if (wild && strings.HasPrefix(code, prefix)) || code == want {
					for _, sc := range c.secs {
						b.WriteString(regListRow(code, c.id, sc))
					}
				}
			}
			b.WriteString("</TABLE></html>")
			w.Write(enc(b.String()))
		case strings.HasSuffix(r.URL.Path, "class_info_2.asp"):
			count("detail")
			w.Write(enc(detail[q.Get("courseid")]))
		default:
			http.NotFound(w, r)
		}
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

func TestRegEnrolment_SumsByTrackAndRemembersTracks(t *testing.T) {
	f := regFixture(t, `ARRAY['CP363205','XX000001']`)
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
	if j.Status != "done" || j.Done != 3 || j.Total != 3 {
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
	if r := got["XX000001"]; r.Error != regErrNotFound {
		t.Errorf("unknown code = %+v, want not-found", r)
	}
	// One search per code; the detail page only for CP363205, a merged code
	// whose tracks our sections cannot tell.
	if *hits != 4 || kindCount(kinds, "detail") != 1 {
		t.Errorf("first run: %d requests (%d detail), want 4 (1 detail)", *hits, kindCount(kinds, "detail"))
	}

	// Within ten minutes the result is reused: no request at all.
	if _, err := s.Start(f.ctx, f.StaffID, f.TermID, RegScopeAll); err != nil {
		t.Fatal(err)
	}
	if *hits != 4 {
		t.Errorf("a fresh result was fetched again (%d requests)", *hits)
	}

	// Later, CP363205's tracks are remembered: one request per code.
	old := time.Now().Add(-time.Hour)
	age := func() {
		s.jobs[f.TermID].FinishedAt = &old
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
	if *hits != 4+3 || kindCount(kinds, "detail") != 1 {
		t.Errorf("second run: %d requests (%d detail in total), want 3 and no new detail page", *hits-4, kindCount(kinds, "detail"))
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
	// CP* (5 codes): one search. SC363001 alone: one search. CP363205's
	// tracks: one detail page.
	if kindCount(kinds, "search") != 1 || kindCount(kinds, "list") != 1 || kindCount(kinds, "detail") != 1 || *hits != 3 {
		t.Errorf("requests: %d search, %d list, %d detail (total %d); want 1/1/1",
			kindCount(kinds, "search"), kindCount(kinds, "list"), kindCount(kinds, "detail"), *hits)
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
