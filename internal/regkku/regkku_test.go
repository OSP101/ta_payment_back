package regkku

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
)

func load(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Pages saved from reg.kku.ac.th on 03/10/2026 (term 2569/1).
func TestParseSections_SplitsTracks(t *testing.T) {
	cases := []struct {
		file, code string
		want       []Section
	}{
		{"class_info_2_SC363001.html", "SC363001", []Section{
			{No: "01", Level: "ปริญญาตรี ภาคปกติ", Open: 85, Enrolled: 73},
			{No: "02", Level: "ปริญญาตรี โครงการพิเศษ", Special: true, Open: 20, Enrolled: 7},
		}},
		{"class_info_2_CP363205.html", "CP363205", []Section{
			{No: "01", Level: "ปริญญาตรี ภาคปกติ", Open: 51, Enrolled: 49},
			{No: "02", Level: "ปริญญาตรี โครงการพิเศษ", Special: true, Open: 45, Enrolled: 41},
		}},
	}
	for _, tc := range cases {
		got, err := ParseSections(load(t, tc.file), tc.code)
		if err != nil {
			t.Fatalf("%s: %v", tc.code, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %+v, want %+v", tc.code, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s[%d] = %+v, want %+v", tc.code, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseSections_WrongCourseIsNotFound(t *testing.T) {
	if _, err := ParseSections(load(t, "class_info_2_SC363001.html"), "CP999999"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestParseSections_ChangedLayoutIsReported(t *testing.T) {
	if _, err := ParseSections("<html>SC363001 ที่นั่ง but no table</html>", "SC363001"); err != ErrLayout {
		t.Fatalf("err = %v, want ErrLayout", err)
	}
}

func TestList_DecodesThaiReadsSeatsAndPacesRequests(t *testing.T) {
	page := load(t, "class_info_1_SC363001.tis620.html")
	var hits int32
	var times []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		times = append(times, time.Now())
		if r.URL.Query().Get("coursecode") == "SC363001" {
			w.Write([]byte(page))
			return
		}
		// The real site answers in TIS-620; so does this fake.
		b, _ := charmap.Windows874.NewEncoder().String("<html>รหัสวิชา: CP999999</html>")
		w.Write([]byte(b))
	}))
	defer srv.Close()
	c := New(srv.URL)
	c.Gap = 150 * time.Millisecond
	c.Jitter = 0
	ctx := context.Background()

	id, secs, err := c.List(ctx, "SC363001", 2569, 1)
	if err != nil || id != "3613665151" {
		t.Fatalf("List = %q, %v", id, err)
	}
	want := []Listed{{CourseID: "3613665151", No: "1", Open: 85, Enrolled: 73}, {CourseID: "3613665151", No: "2", Open: 20, Enrolled: 7}}
	if len(secs) != 2 || secs[0] != want[0] || secs[1] != want[1] {
		t.Fatalf("sections = %+v, want %+v", secs, want)
	}
	if _, _, err := c.List(ctx, "CP999999", 2569, 1); err != ErrNotFound {
		t.Fatalf("missing code: err = %v, want ErrNotFound", err)
	}
	if hits != 2 {
		t.Fatalf("hits = %d", hits)
	}
	if gap := times[1].Sub(times[0]); gap < 140*time.Millisecond {
		t.Errorf("second request came %v after the first, want ≥ Gap", gap)
	}
}

func TestGet_WaitsOutBusyOnceThenGivesUp(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(srv.URL)
	c.Gap, c.Jitter = 0, 0
	if _, _, err := c.List(context.Background(), "SC363001", 2569, 1); err != ErrBusy {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want one request and one retry", hits)
	}
}

// listRow is one search-result row in the registrar's markup.
func listRow(code, id string, sec, open, enrolled int) string {
	return `<TR VALIGN=TOP Class=NormalDetail><TD WIDTH=30></TD><TD BGCOLOR=#F0F0F5>&nbsp;<A HREF=class_info_2.asp?backto=HOME&option=0&courseid=` + id +
		`&acadyear=2569&semester=1&avs5931437=1>` + code + `</A>&nbsp;</TD><TD BGCOLOR=#F0F0F5>&nbsp;Name</TD><TD NOWRAP BGCOLOR=#F0F0F5>&nbsp;3 (2-2-5)</TD>` +
		`<TD ALIGN=RIGHT BGCOLOR=#F0F0F5>` + strconv.Itoa(sec) + `&nbsp;</TD><TD ALIGN=RIGHT BGCOLOR=#F0F0F5>` + strconv.Itoa(open) +
		`&nbsp;</TD><TD ALIGN=RIGHT BGCOLOR=#F0F0F5>` + strconv.Itoa(enrolled) + `&nbsp;</TD><TD ALIGN=RIGHT BGCOLOR=#F0F0F5>` + strconv.Itoa(open-enrolled) +
		`&nbsp;</TD><TD BGCOLOR=#F0F0F5>&nbsp;W</TD><TD></TD></TR>`
}

func TestSearch_FollowsPagesAndSendsFacultyInTIS620(t *testing.T) {
	enc := func(s string) []byte { b, _ := charmap.Windows874.NewEncoder().String(s); return []byte(b) }
	var faculties []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		q, _ := url.ParseQuery(raw)
		faculties = append(faculties, q.Get("facultyid"))
		if q.Get("page") == "" {
			// A course split across pages: CP363205 sec 1 here, sec 2 on page 2.
			w.Write(enc(`<html><TABLE>` + listRow("CP363205", "3613670353", 1, 51, 49) + listRow("CP352201", "11", 1, 50, 19) +
				`</TABLE><A HREF="class_info_1.asp?facultyid=038วิทยาลัยการคอมพิวเตอร์&amp;coursecode=CP*&amp;maxrow=250&amp;page=2&amp;backto=HOME">[หน้าต่อไป]</A></html>`))
			return
		}
		w.Write(enc(`<html><TABLE>` + listRow("CP363205", "3613670353", 2, 45, 41) + `</TABLE>[หน้าก่อน] [หน้าต่อไป]</html>`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	c.Gap, c.Jitter = 0, 0

	got, err := c.Search(context.Background(), "CP*", CollegeOfComputing, 2569, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(faculties) != 2 {
		t.Fatalf("requests = %d, want 2 pages", len(faculties))
	}
	want, _ := charmap.Windows874.NewEncoder().String(CollegeOfComputing)
	for i, f := range faculties {
		if f != want {
			t.Errorf("page %d facultyid = %q, want the TIS-620 bytes", i+1, f)
		}
	}
	cp := got["CP363205"]
	if cp == nil || cp.ID != "3613670353" || len(cp.Sections) != 2 || cp.Sections[1].Enrolled != 41 {
		t.Fatalf("CP363205 = %+v", cp)
	}
	if got["CP352201"] == nil || got["CP352201"].Sections[0].Enrolled != 19 {
		t.Fatalf("CP352201 = %+v", got["CP352201"])
	}
}
