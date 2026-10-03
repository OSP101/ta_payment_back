package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/regkku"
	"ta-payment-back/internal/timeutil"
)

// RegEnrolmentService fetches the real enrolment of a term's courses from the
// registrar (reg.kku.ac.th), for staff to review in the bulk-count dialog
// (requested 03/10/2026). It never writes a count itself: the result is
// handed to the page, which runs it through BulkSetNumStudents — the same
// preview, export lock and budget confirmation as a paste from Excel.
//
// It is built to cost the registrar as little as possible, so it is never
// mistaken for abuse and blocked:
//   - one fetch runs at a time in the whole system, requests one by one,
//     ~1–1.5 s apart, backing off when the registrar says it is busy
//     (regkku.Client);
//   - staff choose the scope: only courses that asked for a TA (the usual
//     need, a fraction of the term) or all of them;
//   - only courses that can still change (not exported) are fetched;
//   - large groups of codes ("CP*", "SC*", old "34*") are read with one
//     wildcard search limited to the college — a whole term is about seven
//     requests — and the rest with one search each (searchGroups);
//   - a code costs no more than that search when the tracks of its
//     class groups are known — from our sections, or remembered from an
//     earlier detail page (reg_kku_section_tracks); see fetchCode;
//   - each code's answer is reused for regResultTTL, across scopes: fetching
//     "requested" and then "all" only fetches the rest.
//
// The fetch outlives the HTTP request that starts it (a term is a few
// minutes of paced requests); the page polls Status.
type RegEnrolmentService struct {
	pool *pgxpool.Pool
	reg  *regkku.Client

	mu    sync.Mutex
	jobs  map[uuid.UUID]*RegEnrolmentJob // the latest per term
	codes map[regCodeKey]regCached       // per-code answers, see regResultTTL
	busy  bool
}

type regCodeKey struct {
	term uuid.UUID
	code string
}

type regCached struct {
	row RegEnrolmentRow
	at  time.Time
}

// Fetch scopes.
const (
	RegScopeRequested = "requested" // courses with a submitted or approved TA request
	RegScopeAll       = "all"
)

const regResultTTL = 10 * time.Minute

// RegEnrolmentRow is one code's registered students, by track.
type RegEnrolmentRow struct {
	Code     string `json:"code"`
	Regular  int    `json:"regular"`
	Special  int    `json:"special"`
	Sections string `json:"sections,omitempty"` // "01 ปกติ 73, 02 พิเศษ 7" — what the sums came from
	Error    string `json:"error,omitempty"`
}

// RegEnrolmentJob is what the page polls.
type RegEnrolmentJob struct {
	Scope      string            `json:"scope"`
	Status     string            `json:"status"` // running | done | stopped | failed
	Done       int               `json:"done"`
	Total      int               `json:"total"`
	Rows       []RegEnrolmentRow `json:"rows"`
	Skipped    int               `json:"skipped_exported"`
	Reused     int               `json:"reused"` // answered from the 10-minute cache, no request
	Error      string            `json:"error,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at,omitempty"`

	stop     context.CancelFunc // set while running; Stop calls it
	stopping bool
}

func NewRegEnrolmentService(pool *pgxpool.Pool, base string) *RegEnrolmentService {
	return &RegEnrolmentService{
		pool: pool, reg: regkku.New(base),
		jobs: map[uuid.UUID]*RegEnrolmentJob{}, codes: map[regCodeKey]regCached{},
	}
}

func (s *RegEnrolmentService) snapshot(j *RegEnrolmentJob) *RegEnrolmentJob {
	c := *j
	c.Rows = append([]RegEnrolmentRow(nil), j.Rows...)
	return &c
}

// Status returns the term's current or last fetch, nil when there is none.
func (s *RegEnrolmentService) Status(ctx context.Context, actor, termID uuid.UUID) (*RegEnrolmentJob, error) {
	if err := s.mayFetch(ctx, actor); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.jobs[termID]; j != nil {
		return s.snapshot(j), nil
	}
	return nil, nil
}

func (s *RegEnrolmentService) mayFetch(ctx context.Context, actor uuid.UUID) error {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return err
	}
	if !priv {
		return Forbidden("เฉพาะเจ้าหน้าที่เท่านั้นที่ดึงจำนวนนักศึกษาจากระบบทะเบียนได้")
	}
	return nil
}

// Start begins fetching the term's courses in scope, or returns the term's
// fetch already running, or the same scope's result if it is still fresh.
func (s *RegEnrolmentService) Start(ctx context.Context, actor, termID uuid.UUID, scope string) (*RegEnrolmentJob, error) {
	if err := s.mayFetch(ctx, actor); err != nil {
		return nil, err
	}
	if scope != RegScopeAll {
		scope = RegScopeRequested
	}
	var year, semester int
	if err := s.pool.QueryRow(ctx,
		`SELECT academic_year, semester FROM academic_terms WHERE id = $1`, termID).Scan(&year, &semester); err != nil {
		return nil, NotFound("ไม่พบภาคการศึกษานี้")
	}

	s.mu.Lock()
	if j := s.jobs[termID]; j != nil {
		if j.Status == "running" ||
			(j.Scope == scope && j.Status == "done" && j.FinishedAt != nil && time.Since(*j.FinishedAt) < regResultTTL) {
			snap := s.snapshot(j)
			s.mu.Unlock()
			return snap, nil
		}
	}
	if s.busy {
		s.mu.Unlock()
		return nil, Conflict("กำลังดึงข้อมูลจากระบบทะเบียนของภาคอื่นอยู่ กรุณารอสักครู่แล้วลองใหม่")
	}
	s.busy = true
	s.mu.Unlock()

	codes, skipped, err := s.termCodes(ctx, termID, scope)
	if err != nil {
		s.release()
		return nil, err
	}
	if len(codes) == 0 {
		s.release()
		if scope == RegScopeRequested {
			return nil, Invalid("ยังไม่มีวิชาที่ขอ TA และยังแก้จำนวนนักศึกษาได้ในภาคนี้ ลองเลือกดึงทุกวิชา")
		}
		return nil, Invalid("ไม่มีรายวิชาที่ยังแก้จำนวนนักศึกษาได้ในภาคนี้ (ส่งออกแล้วทั้งหมด)")
	}

	j := &RegEnrolmentJob{
		Scope: scope, Status: "running", Total: len(codes), Skipped: skipped,
		Rows: []RegEnrolmentRow{}, StartedAt: timeutil.Now(),
	}
	s.mu.Lock()
	s.jobs[termID] = j
	snap := s.snapshot(j)
	s.mu.Unlock()

	go s.run(termID, j, codes, year, semester)
	return snap, nil
}

func (s *RegEnrolmentService) release() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
}

// termCodes lists every code (primary and merged) of the term's courses whose
// counts can still change, and how many courses were left out as exported.
// scope "requested" keeps the courses the staff list shows as "ขอ TA แล้ว":
// a submitted or approved TA request.
func (s *RegEnrolmentService) termCodes(ctx context.Context, termID uuid.UUID, scope string) ([]string, int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT code, COALESCE(alt_codes, '{}'), exported_at IS NOT NULL
		  FROM teaching_courses tc
		 WHERE term_id = $1
		   AND ($2 = 'all' OR EXISTS (
		         SELECT 1 FROM ta_requests r
		          WHERE r.teaching_course_id = tc.id AND r.status IN ('submitted', 'approved')))`,
		termID, scope)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var codes []string
	skipped := 0
	for rows.Next() {
		var code string
		var alts []string
		var exported bool
		if err := rows.Scan(&code, &alts, &exported); err != nil {
			return nil, 0, err
		}
		if exported {
			skipped++
			continue
		}
		for _, c := range append([]string{code}, alts...) {
			c = normalizeCode(c)
			if c != "" && !seen[c] {
				seen[c] = true
				codes = append(codes, c)
			}
		}
	}
	sort.Strings(codes)
	return codes, skipped, rows.Err()
}

func (s *RegEnrolmentService) run(termID uuid.UUID, j *RegEnrolmentJob, codes []string, year, semester int) {
	defer s.release()
	// Paced at ~1.25 s a request, a whole term of 150 codes is 3–6 minutes;
	// the cap only stops a hung registrar from holding the single slot forever.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	s.mu.Lock()
	j.stop = cancel
	s.mu.Unlock()
	stopped := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return j.stopping
	}

	listed := s.searchGroups(ctx, termID, codes, year, semester)

	consecutiveNetErr := 0
	for _, code := range codes {
		if stopped() {
			s.finish(j, "stopped", "")
			return
		}
		key := regCodeKey{termID, code}
		s.mu.Lock()
		c, ok := s.codes[key]
		s.mu.Unlock()
		reused := ok && time.Since(c.at) < regResultTTL
		row := c.row
		if !reused {
			row = s.fetchCode(ctx, termID, code, listed[code], year, semester)
		}
		// Stopped mid-request: that code's answer is the cancellation, not
		// the registrar's — leave it out rather than report it as an error.
		if stopped() {
			s.finish(j, "stopped", "")
			return
		}

		s.mu.Lock()
		j.Rows = append(j.Rows, row)
		j.Done++
		if reused {
			j.Reused++
		} else if row.Error == "" || row.Error == regErrNotFound {
			s.codes[key] = regCached{row: row, at: time.Now()}
		}
		s.mu.Unlock()

		// The registrar down, unreachable or asking us to slow down: stop
		// after a few in a row rather than keep knocking.
		if row.Error == regErrNetwork {
			consecutiveNetErr++
			if consecutiveNetErr >= 3 {
				s.finish(j, "failed", "ติดต่อระบบทะเบียน (reg.kku.ac.th) ไม่ได้ หรือระบบทะเบียนขอให้ชะลอ จึงหยุดดึงไว้ก่อน กรุณาลองใหม่ภายหลัง")
				return
			}
		} else {
			consecutiveNetErr = 0
		}
		if ctx.Err() != nil {
			s.finish(j, "failed", "ดึงข้อมูลนานเกินกำหนด กรุณาลองใหม่ภายหลัง")
			return
		}
	}
	s.finish(j, "done", "")
}

func (s *RegEnrolmentService) finish(j *RegEnrolmentJob, status, msg string) {
	now := timeutil.Now()
	s.mu.Lock()
	j.Status, j.Error, j.FinishedAt, j.stop = status, msg, &now, nil
	s.mu.Unlock()
}

// Stop ends the term's running fetch at once — the request in flight is
// abandoned, nothing more is sent to the registrar. What was fetched so far
// stays in the job (status "stopped") for staff to use, and each fetched
// code's answer stays cached, so starting again only fetches the rest.
func (s *RegEnrolmentService) Stop(ctx context.Context, actor, termID uuid.UUID) (*RegEnrolmentJob, error) {
	if err := s.mayFetch(ctx, actor); err != nil {
		return nil, err
	}
	s.mu.Lock()
	j := s.jobs[termID]
	if j == nil || j.Status != "running" {
		s.mu.Unlock()
		return nil, Invalid("ไม่มีการดึงข้อมูลที่กำลังทำงานอยู่")
	}
	j.stopping = true
	if j.stop != nil {
		j.stop()
	}
	s.mu.Unlock()

	// The loop notices within one step; report the settled state.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := j.Status != "running"
		snap := s.snapshot(j)
		s.mu.Unlock()
		if done {
			return snap, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(j), nil
}

const (
	regErrNetwork  = "ติดต่อระบบทะเบียนไม่ได้"
	regErrNotFound = "ไม่พบรหัสวิชานี้ในระบบทะเบียนภาคนี้"
	regErrLayout   = "อ่านหน้าระบบทะเบียนไม่ได้ (หน้าเว็บอาจเปลี่ยนรูปแบบ)"
)

// fetchCode reads one code's enrolment by track.
//
// One request, the registrar's search page, gives every class group with its
// "ลง" count — but not whether a group is regular or special. That comes from
// what we already know (knownTracks): our own sections for a course's own
// code, and tracks remembered from an earlier detail page. Only when a group
// is still unknown — a merged code, whose sections were folded away, or a
// group the registrar opened after our import — is the detail page read
// (a second request), and its tracks are remembered for next time.
//
// pre is the code's rows from a wildcard search (searchGroups), nil when the
// code was not covered by one — then the code is searched on its own.
func (s *RegEnrolmentService) fetchCode(ctx context.Context, termID uuid.UUID, code string, pre *regkku.Course, year, semester int) RegEnrolmentRow {
	row := RegEnrolmentRow{Code: code}
	var courseID string
	var listed []regkku.Listed
	if pre != nil {
		courseID, listed = pre.ID, pre.Sections
	} else {
		var err error
		courseID, listed, err = s.reg.List(ctx, code, year, semester)
		if err != nil {
			row.Error = regFetchError(code, err)
			return row
		}
	}
	known, err := s.knownTracks(ctx, termID, code, year, semester)
	if err != nil {
		log.Printf("reg enrolment: tracks of %s: %v", code, err)
		known = map[string]bool{}
	}
	// The detail page of every registrar course id that has a group we
	// cannot place — usually one id, two when the registrar lists an old
	// revision's group under the same code.
	var ids []string
	seenID := map[string]bool{}
	for _, l := range listed {
		if _, ok := known[l.No]; ok {
			continue
		}
		id := l.CourseID
		if id == "" {
			id = courseID
		}
		if !seenID[id] {
			seenID[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		secs, err := s.reg.Sections(ctx, id, code, year, semester)
		if err != nil {
			row.Error = regFetchError(code, err)
			return row
		}
		for _, sec := range secs {
			no := strings.TrimLeft(sec.No, "0")
			known[no] = sec.Special
			if _, err := s.pool.Exec(ctx, `
				INSERT INTO reg_kku_section_tracks (academic_year, semester, code, sec_no, special)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (academic_year, semester, code, sec_no)
				DO UPDATE SET special = EXCLUDED.special, fetched_at = NOW()`,
				year, semester, code, no, sec.Special); err != nil {
				log.Printf("reg enrolment: remember %s sec %s: %v", code, no, err)
			}
		}
	}

	parts := make([]string, 0, len(listed))
	for _, l := range listed {
		special, ok := known[l.No]
		if !ok {
			// Listed on the search page yet absent from the detail page:
			// the two disagree, so no number is safer than a guessed track.
			row.Error = regErrLayout
			return row
		}
		track := "ปกติ"
		if special {
			row.Special += l.Enrolled
			track = "พิเศษ"
		} else {
			row.Regular += l.Enrolled
		}
		parts = append(parts, fmt.Sprintf("%s %s %d", l.No, track, l.Enrolled))
	}
	row.Sections = strings.Join(parts, ", ")
	return row
}

// regWildcardMin is the group size from which one wildcard search ("CP*",
// a few pages of 250 rows) costs the registrar less than searching each code.
// Measured 03/10/2026: the college's CP* and SC* are 3 pages each, its 34*
// one page — against 124 and 103 single searches.
const regWildcardMin = 5

// regPattern is the wildcard group of a code: its letter prefix ("CP*",
// "SC*") or, for the old all-digit codes, its first two digits ("34*").
func regPattern(code string) string {
	i := 0
	for i < len(code) && code[i] >= 'A' && code[i] <= 'Z' {
		i++
	}
	if i > 0 {
		return code[:i] + "*"
	}
	if len(code) >= 2 {
		return code[:2] + "*"
	}
	return ""
}

// searchGroups runs one wildcard search, limited to the College of
// Computing, for each group of still-needed codes large enough to be worth
// it, and returns the rows found by code. Codes it does not return (fewer
// in their group, or owned by another faculty) are searched one by one. A
// failed group search is logged and simply falls back the same way.
func (s *RegEnrolmentService) searchGroups(ctx context.Context, termID uuid.UUID, codes []string, year, semester int) map[string]*regkku.Course {
	groups := map[string]int{}
	s.mu.Lock()
	for _, c := range codes {
		if cached, ok := s.codes[regCodeKey{termID, c}]; ok && time.Since(cached.at) < regResultTTL {
			continue
		}
		if p := regPattern(c); p != "" {
			groups[p]++
		}
	}
	s.mu.Unlock()

	out := map[string]*regkku.Course{}
	patterns := make([]string, 0, len(groups))
	for p, n := range groups {
		if n >= regWildcardMin {
			patterns = append(patterns, p)
		}
	}
	sort.Strings(patterns)
	for _, p := range patterns {
		found, err := s.reg.Search(ctx, p, regkku.CollegeOfComputing, year, semester, 10)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("reg enrolment: search %s: %v (falling back to single searches)", p, err)
			}
			continue
		}
		for k, v := range found {
			out[k] = v
		}
	}
	return out
}

// knownTracks maps a code's class-group numbers ("1", no leading zero) to
// special/regular from what needs no request: tracks remembered from the
// registrar's detail page first (the registrar's own word), then our
// sections — the course's own sections for its primary code, and any
// not-yet-folded "SC361002-3" section for a merged code.
func (s *RegEnrolmentService) knownTracks(ctx context.Context, termID uuid.UUID, code string, year, semester int) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := s.pool.Query(ctx, `
		SELECT ltrim(CASE WHEN s.course_code IS NULL THEN s.sec_no
		                  ELSE substr(s.sec_no, length(s.course_code) + 2) END, '0'),
		       s.track = 'special'
		  FROM sections s
		  JOIN teaching_courses tc ON tc.id = s.teaching_course_id
		 WHERE tc.term_id = $1
		   AND ((s.course_code IS NULL AND upper(tc.code) = $2) OR upper(s.course_code) = $2)`,
		termID, code)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var no string
		var special bool
		if err := rows.Scan(&no, &special); err != nil {
			rows.Close()
			return nil, err
		}
		out[no] = special
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `
		SELECT sec_no, special FROM reg_kku_section_tracks
		 WHERE academic_year = $1 AND semester = $2 AND code = $3`, year, semester, code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var no string
		var special bool
		if err := rows.Scan(&no, &special); err != nil {
			return nil, err
		}
		out[no] = special
	}
	return out, rows.Err()
}

func regFetchError(code string, err error) string {
	switch {
	case errors.Is(err, regkku.ErrNotFound):
		return regErrNotFound
	case errors.Is(err, regkku.ErrLayout):
		log.Printf("reg enrolment: %s: page layout not recognised", code)
		return regErrLayout
	default:
		if !errors.Is(err, context.Canceled) { // a stop, not a failure
			log.Printf("reg enrolment: %s: %v", code, err)
		}
		return regErrNetwork
	}
}
