// Package regkku reads class information from the university registrar's
// public pages (reg.kku.ac.th/registrar): which sections a course opens in a
// term, their track, and how many students registered.
//
// There is no API. The pages are classic ASP rendered in TIS-620 (Windows-874)
// and are read here the way a browser reads them, with rules that keep the
// load on the registrar's server at the level of one person clicking through
// it, so it never looks like abuse:
//
//   - one request at a time per Client, spaced by Gap plus a random Jitter;
//   - a "busy" answer (429/503) is waited out (Retry-After, at most a minute)
//     and asked once more, never hammered;
//   - one search request (List) gives a code's class groups and seats; the
//     detail page (Sections), the only place that tells regular from
//     special, is fetched only when the caller does not already know the
//     groups' tracks.
//
// Production reaches reg.kku.ac.th through the campus DNS even without an
// outside internet route, so no proxy setting exists.
package regkku

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding/charmap"
)

// DefaultBase is the registrar application's root.
const DefaultBase = "https://reg.kku.ac.th/registrar"

// ErrNotFound means the registrar has no such code open in that term.
var ErrNotFound = errors.New("regkku: course not found")

// ErrLayout means the page came back but no longer looks the way this package
// parses it — the registrar changed the page. Reported, never guessed around.
var ErrLayout = errors.New("regkku: unexpected page layout")

// Section is one class group as the registrar lists it.
type Section struct {
	No       string // "01"
	Level    string // the heading it sits under, e.g. "ปริญญาตรี โครงการพิเศษ"
	Special  bool   // under a โครงการพิเศษ / ภาคพิเศษ heading
	Open     int    // seats opened
	Enrolled int    // registered (ลง)
}

// Client talks to the registrar. The zero value is not usable; use New.
type Client struct {
	Base string
	HTTP *http.Client
	// Gap is the minimum time between two requests from this client; up to
	// Jitter more is added at random so the requests have no machine rhythm.
	Gap    time.Duration
	Jitter time.Duration
	// MaxBackoff caps how long a 429/503 answer is waited out.
	MaxBackoff time.Duration

	mu   sync.Mutex
	last time.Time
}

// New returns a client for base ("" = DefaultBase) with conservative pacing.
func New(base string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return &Client{
		Base:       strings.TrimRight(base, "/"),
		HTTP:       &http.Client{Timeout: 20 * time.Second},
		Gap:        time.Second,
		Jitter:     500 * time.Millisecond,
		MaxBackoff: time.Minute,
	}
}

// ErrBusy means the registrar asked us to slow down and still did after a
// wait. The caller stops instead of continuing.
var ErrBusy = errors.New("regkku: registrar busy")

// get fetches one page and decodes it to UTF-8, waiting out the gap first.
// The lock is held for the whole request so requests never overlap.
func (c *Client) get(ctx context.Context, path string, q url.Values) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; ; attempt++ {
		page, retryAfter, err := c.once(ctx, path, q)
		if !errors.Is(err, ErrBusy) || attempt > 0 {
			return page, err
		}
		if retryAfter > c.MaxBackoff {
			retryAfter = c.MaxBackoff
		}
		select {
		case <-time.After(retryAfter):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func (c *Client) once(ctx context.Context, path string, q url.Values) (string, time.Duration, error) {
	wait := c.Gap - time.Since(c.last)
	if c.Jitter > 0 {
		wait += time.Duration(rand.Int64N(int64(c.Jitter)))
	}
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return "", 0, ctx.Err()
		}
	}
	defer func() { c.last = time.Now() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/"+path+"?"+q.Encode(), nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "COCO-TAS/1.0 (College of Computing KKU; enrolment check)")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable {
		after := 30 * time.Second
		if n, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && n >= 0 {
			after = time.Duration(n) * time.Second
		}
		return "", after, ErrBusy
	}
	if res.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("regkku: %s answered %d", path, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(charmap.Windows874.NewDecoder().Reader(res.Body), 4<<20))
	if err != nil {
		return "", 0, err
	}
	return string(body), 0, nil
}

var courseLinkRe = regexp.MustCompile(`(?i)<a\s+href=["']?class_info_2\.asp\?[^>]*?courseid=(\d+)[^>]*>\s*([^<]+?)\s*</a>`)

// listCellRe is one numeric cell of a search-result row: "<TD ALIGN=RIGHT …>85&nbsp;</TD>".
var listCellRe = regexp.MustCompile(`(?i)<TD ALIGN=RIGHT[^>]*>\s*(-?\d+)\s*(?:&nbsp;)?\s*</TD>`)

// Listed is one row of the registrar's search result: a class group with
// its seats, but no track — the search page does not say regular or
// special; only the detail page (Sections) does.
type Listed struct {
	// CourseID is the registrar's id of the course this group belongs to.
	// Usually one per code, but not always: CP424732 in 2569/1 lists sec 1
	// under a closed older revision and sec 2 under the current one.
	CourseID string
	No       string // "1" — as the search page prints it, no leading zero
	Open     int
	Enrolled int
}

// List runs the registrar's class search for one code and term and returns
// the registrar's course id (the detail page's key) and every class group
// with its seats. One request. The search page is the form at
// class_info.asp; it is sent as GET, which the page accepts identically.
func (c *Client) List(ctx context.Context, code string, year, semester int) (string, []Listed, error) {
	page, err := c.get(ctx, "class_info_1.asp", url.Values{
		"avs5931099": {"2"}, "backto": {"home"},
		"coursestatus": {"O00"}, "facultyid": {"all"}, "maxrow": {"250"},
		"Acadyear": {strconv.Itoa(year)}, "Semester": {strconv.Itoa(semester)},
		"coursecode": {code}, "coursename": {""}, "cmd": {"2"},
	})
	if err != nil {
		return "", nil, err
	}
	return ParseList(page, code)
}

// ParseList is List without the fetch, for tests.
func ParseList(page, code string) (string, []Listed, error) {
	all, err := ParseListAll(page)
	if err != nil {
		return "", nil, err
	}
	for k, c := range all {
		if strings.EqualFold(k, code) {
			return c.ID, c.Sections, nil
		}
	}
	if !strings.Contains(page, "class_info_1.asp") && !strings.Contains(page, "รหัสวิชา") {
		return "", nil, ErrLayout
	}
	return "", nil, ErrNotFound
}

// Course is one code's search result: the registrar's course id and its
// class groups with seats.
type Course struct {
	ID       string
	Sections []Listed
}

// ParseListAll reads every row of a search-result page, keyed by code
// (upper case).
func ParseListAll(page string) (map[string]*Course, error) {
	out := map[string]*Course{}
	for _, row := range rowSplitRe.Split(page, -1) {
		m := courseLinkRe.FindStringSubmatch(row)
		if m == nil {
			continue
		}
		// Group, open, enrolled, remaining: the row's four right-aligned
		// numbers after the link (credits is NOWRAP, not ALIGN=RIGHT).
		cells := listCellRe.FindAllStringSubmatch(row, -1)
		if len(cells) < 4 {
			return nil, ErrLayout
		}
		code := strings.ToUpper(strings.TrimSpace(m[2]))
		c := out[code]
		if c == nil {
			c = &Course{ID: m[1]}
			out[code] = c
		}
		open, _ := strconv.Atoi(cells[1][1])
		enrolled, _ := strconv.Atoi(cells[2][1])
		c.Sections = append(c.Sections, Listed{CourseID: m[1], No: strings.TrimLeft(cells[0][1], "0"), Open: open, Enrolled: enrolled})
	}
	return out, nil
}

// CollegeOfComputing is the search form's owner value for วิทยาลัยการคอมพิวเตอร์
// (its <option value>, number and name together).
const CollegeOfComputing = "038วิทยาลัยการคอมพิวเตอร์"

// cp874 turns a value into the bytes the registrar's TIS-620 pages expect in
// a query string. ASCII is unchanged.
func cp874(v string) string {
	b, err := charmap.Windows874.NewEncoder().String(v)
	if err != nil {
		return v
	}
	return b
}

var nextPageRe = regexp.MustCompile(`(?i)<a[^>]*href=["']?class_info_1\.asp\?([^"' >]*page=\d+[^"' >]*)["']?[^>]*>\s*\[?หน้าต่อไป`)

// Search runs one wildcard search ("CP*", "34*") limited to one owner
// (faculty, e.g. CollegeOfComputing) and follows the result pages, at most
// maxPages. A term of the college's CP courses is 3 pages of 250 rows —
// three requests instead of one per code. Rows of every code are returned.
func (c *Client) Search(ctx context.Context, pattern, faculty string, year, semester, maxPages int) (map[string]*Course, error) {
	q := url.Values{
		"avs5931099": {"2"}, "backto": {"home"},
		"coursestatus": {"O00"}, "facultyid": {cp874(faculty)}, "maxrow": {"250"},
		"Acadyear": {strconv.Itoa(year)}, "Semester": {strconv.Itoa(semester)},
		"coursecode": {pattern}, "coursename": {""}, "cmd": {"2"},
	}
	out := map[string]*Course{}
	for page := 0; page < maxPages; page++ {
		body, err := c.get(ctx, "class_info_1.asp", q)
		if err != nil {
			return nil, err
		}
		rows, err := ParseListAll(body)
		if err != nil {
			return nil, err
		}
		for k, v := range rows {
			if have := out[k]; have != nil {
				have.Sections = append(have.Sections, v.Sections...)
			} else {
				out[k] = v
			}
		}
		m := nextPageRe.FindStringSubmatch(body)
		if m == nil {
			return out, nil
		}
		// The next link carries the paging keys (page, coursecodeSerch,
		// LaststartSerch); its values are re-encoded to TIS-620 because the
		// page was decoded to UTF-8.
		q = url.Values{}
		for _, kv := range strings.Split(html.UnescapeString(m[1]), "&") {
			k, v, _ := strings.Cut(kv, "=")
			if uv, err := url.QueryUnescape(v); err == nil {
				v = uv
			}
			q.Set(k, cp874(v))
		}
	}
	return out, nil
}

var (
	rowSplitRe = regexp.MustCompile(`(?i)<tr[\s>]`)
	headingRe  = regexp.MustCompile(`<B>\s*(ปริญญา[^<]*?)\s*</B>`)
	groupRe    = regexp.MustCompile(`(?i)Class=NormalDetail>(?:&nbsp;|\s)*(\d{1,3})\s*</TD>`)
	seatsRe    = regexp.MustCompile(`(?i)<TD ALIGN=RIGHT>\s*(\d+)\s*</TD>\s*<TD ALIGN=CENTER>\s*(\d+)\s*</TD>\s*<TD ALIGN=LEFT>\s*(-?\d+)\s*</TD>`)
)

// Sections reads every class group of a course in a term from its detail
// page. code is checked against the page so a stale course id (the registrar
// renumbered the course) is caught instead of read as another course.
func (c *Client) Sections(ctx context.Context, courseID, code string, year, semester int) ([]Section, error) {
	page, err := c.get(ctx, "class_info_2.asp", url.Values{
		"backto": {"HOME"}, "option": {"0"}, "courseid": {courseID},
		"acadyear": {strconv.Itoa(year)}, "semester": {strconv.Itoa(semester)},
	})
	if err != nil {
		return nil, err
	}
	return ParseSections(page, code)
}

// ParseSections is Sections without the fetch, for tests.
func ParseSections(page, code string) ([]Section, error) {
	if !strings.Contains(strings.ToUpper(page), strings.ToUpper(code)) {
		return nil, ErrNotFound
	}
	if !strings.Contains(page, "ที่นั่ง") {
		// The course exists but the term has no class table: not offered.
		if strings.Contains(page, "ปีการศึกษา") {
			return nil, nil
		}
		return nil, ErrLayout
	}
	var out []Section
	level := ""
	for _, row := range rowSplitRe.Split(page, -1) {
		if m := headingRe.FindStringSubmatch(row); m != nil {
			level = strings.Join(strings.Fields(m[1]), " ")
			continue
		}
		g := groupRe.FindStringSubmatch(row)
		s := seatsRe.FindStringSubmatch(row)
		if g == nil || s == nil {
			continue // a second meeting time, the lecturer line, notes…
		}
		open, _ := strconv.Atoi(s[1])
		enrolled, _ := strconv.Atoi(s[2])
		out = append(out, Section{
			No: g[1], Level: level,
			Special:  strings.Contains(level, "พิเศษ"),
			Open:     open,
			Enrolled: enrolled,
		})
	}
	if len(out) == 0 {
		return nil, ErrLayout
	}
	return out, nil
}
