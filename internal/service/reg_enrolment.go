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

// RegEnrolmentService reads the selected term from REG's guest JSON API.
// One job runs at a time; paced prefix searches and a ten-minute per-code
// cache reduce traffic. Results still require staff preview and confirmation.
type RegEnrolmentService struct {
	pool        *pgxpool.Pool
	reg         *regkku.APIClient
	legacy      *regkku.Client
	healthMu    sync.Mutex
	health      []regkku.Availability
	healthUntil time.Time

	mu    sync.Mutex
	jobs  map[regJobKey]*RegEnrolmentJob // the latest per term
	codes map[regCodeKey]regCached       // per-code answers, see regResultTTL
	busy  bool
}

type regJobKey struct {
	term   uuid.UUID
	source string
}

type regCodeKey struct {
	source         string
	term           uuid.UUID
	code           string
	year, semester int
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
	// SectionCounts is the same per section, sent back with the save so each
	// section — not just the course — takes its real enrolment.
	SectionCounts []SectionCount       `json:"section_counts,omitempty"`
	Error         string               `json:"error,omitempty"`
	Problem       *regkku.Availability `json:"problem,omitempty"`
}

// RegEnrolmentJob is what the page polls.
type RegEnrolmentJob struct {
	Source     string               `json:"source"`
	Problem    *regkku.Availability `json:"problem,omitempty"`
	Year       int                  `json:"academic_year"`
	Semester   int                  `json:"semester"`
	Scope      string               `json:"scope"`
	Status     string               `json:"status"` // running | done | stopped | failed
	Done       int                  `json:"done"`
	Total      int                  `json:"total"`
	Rows       []RegEnrolmentRow    `json:"rows"`
	Skipped    int                  `json:"skipped_exported"`
	Reused     int                  `json:"reused"` // answered from the 10-minute cache, no request
	Error      string               `json:"error,omitempty"`
	StartedAt  time.Time            `json:"started_at"`
	FinishedAt *time.Time           `json:"finished_at,omitempty"`

	stop       context.CancelFunc // set while running; Stop calls it
	stopping   bool
	codeGroups [][]string
}

func NewRegEnrolmentService(pool *pgxpool.Pool, base string, legacyBase ...string) *RegEnrolmentService {
	oldBase := ""
	if len(legacyBase) > 0 {
		oldBase = legacyBase[0]
	}
	return &RegEnrolmentService{
		legacy: regkku.New(oldBase),
		pool:   pool, reg: regkku.NewAPI(base),
		jobs: map[regJobKey]*RegEnrolmentJob{}, codes: map[regCodeKey]regCached{},
	}
}

func (s *RegEnrolmentService) snapshot(j *RegEnrolmentJob) *RegEnrolmentJob {
	c := *j
	c.Rows = append([]RegEnrolmentRow(nil), j.Rows...)
	return &c
}

// Status returns the term's current or last fetch, nil when there is none.
func (s *RegEnrolmentService) Status(ctx context.Context, actor, termID uuid.UUID, selected ...string) (*RegEnrolmentJob, error) {
	source, err := regSource(selected)
	if err != nil {
		return nil, err
	}
	if err := s.mayFetch(ctx, actor); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.jobs[regJobKey{termID, source}]; j != nil {
		var year, semester int
		if err := s.pool.QueryRow(ctx, `SELECT academic_year, semester FROM academic_terms WHERE id = $1`, termID).Scan(&year, &semester); err != nil {
			return nil, NotFound("ไม่พบภาคการศึกษานี้")
		}
		// Running jobs remain stoppable; completed old-term data is hidden.
		if j.Status != "running" && (j.Year != year || j.Semester != semester) {
			return nil, nil
		}
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
func (s *RegEnrolmentService) Start(ctx context.Context, actor, termID uuid.UUID, scope string, selected ...string) (*RegEnrolmentJob, error) {
	source, err := regSource(selected)
	if err != nil {
		return nil, err
	}
	if err := s.mayFetch(ctx, actor); err != nil {
		return nil, err
	}
	if err := s.sourceAllowed(source); err != nil {
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
	if j := s.jobs[regJobKey{termID, source}]; j != nil {
		if (j.Status == "running" && j.Year == year && j.Semester == semester) ||
			(j.Year == year && j.Semester == semester && j.Scope == scope && j.Status == "done" && regRowsComplete(j.Rows) && j.FinishedAt != nil && time.Since(*j.FinishedAt) < regResultTTL) {
			snap := s.snapshot(j)
			s.mu.Unlock()
			return snap, nil
		}
	}
	if s.busy {
		s.mu.Unlock()
		return nil, Conflict("กำลังดึงข้อมูลจากระบบทะเบียนอยู่ กรุณารอให้งานเดิมเสร็จหรือหยุดงานเดิมก่อน")
	}
	s.busy = true
	s.mu.Unlock()

	codes, skipped, groups, err := s.termCodes(ctx, termID, scope)
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
		Source: source, Year: year, Semester: semester, Scope: scope, Status: "running", Total: len(codes), Skipped: skipped,
		Rows: []RegEnrolmentRow{}, StartedAt: timeutil.Now(), codeGroups: groups,
	}
	s.mu.Lock()
	s.jobs[regJobKey{termID, source}] = j
	snap := s.snapshot(j)
	s.mu.Unlock()

	if !backgroundJobs.start(func(parent context.Context) { s.run(parent, termID, j, codes, year, semester) }) {
		s.mu.Lock()
		delete(s.jobs,regJobKey{termID,source})
		s.busy=false
		s.mu.Unlock()
		return nil,Conflict("ระบบกำลังประมวลผลงานเบื้องหลัง กรุณาลองใหม่ภายหลัง")
	}
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
func (s *RegEnrolmentService) termCodes(ctx context.Context, termID uuid.UUID, scope string) ([]string, int, [][]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT code, COALESCE(alt_codes, '{}'), exported_at IS NOT NULL
		  FROM teaching_courses tc
		 WHERE term_id = $1
		   AND ($2 = 'all' OR EXISTS (
		         SELECT 1 FROM ta_requests r
		          WHERE r.teaching_course_id = tc.id AND r.status IN ('submitted', 'approved')))`,
		termID, scope)
	if err != nil {
		return nil, 0, nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var codes []string
	var groups [][]string
	skipped := 0
	for rows.Next() {
		var code string
		var alts []string
		var exported bool
		if err := rows.Scan(&code, &alts, &exported); err != nil {
			return nil, 0, nil, err
		}
		if exported {
			skipped++
			continue
		}
		var group []string
		for _, c := range append([]string{code}, alts...) {
			c = normalizeCode(c)
			if c != "" {
				group = append(group, c)
			}
			if c != "" && !seen[c] {
				seen[c] = true
				codes = append(codes, c)
			}
		}
		groups = append(groups, group)
	}
	sort.Strings(codes)
	return codes, skipped, groups, rows.Err()
}

func (s *RegEnrolmentService) run(parent context.Context, termID uuid.UUID, j *RegEnrolmentJob, codes []string, year, semester int) {
	// Paced at ~1.25 s a request, a whole term of 150 codes is 3–6 minutes;
	// the cap only stops a hung registrar from holding the single slot forever.
	ctx, cancel := context.WithTimeout(parent, 20*time.Minute)
	defer cancel()
	s.mu.Lock()
	j.stop = cancel
	if j.stopping {
		cancel()
	}
	s.mu.Unlock()
	stopped := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return j.stopping || ctx.Err() != nil
	}

	source := j.Source
	listed, groupErrors := s.searchGroups(ctx, termID, codes, year, semester, source)

	consecutiveNetErr := 0
	for _, code := range codes {
		if stopped() {
			s.finish(j, "stopped", "")
			return
		}
		key := regCodeKey{term: termID, code: code, year: year, semester: semester, source: source}
		s.mu.Lock()
		c, ok := s.codes[key]
		s.mu.Unlock()
		reused := ok && time.Since(c.at) < regResultTTL
		row := c.row
		if !reused {
			if err := groupErrors[code]; err != nil {
				row = regFailure(code, source, err)
			} else {
				row = s.fetchCode(ctx, termID, code, listed[code], year, semester, source)
			}
		}
		// Stopped mid-request: that code's answer is the cancellation, not
		// the registrar's — leave it out rather than report it as an error.
		if stopped() {
			s.finish(j, "stopped", "")
			return
		}

		if row.Problem != nil {
			s.rememberSourceProblem(*row.Problem)
		}
		s.mu.Lock()
		if row.Problem != nil {
			j.Problem = row.Problem
		}
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
				s.finish(j, "failed", "ติดต่อระบบทะเบียนที่เลือกไม่ได้ หรือระบบทะเบียนขอให้ชะลอ จึงหยุดดึงไว้ก่อน กรุณาลองใหม่ภายหลัง")
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
	s.healthMu.Lock()
	s.healthUntil = time.Time{}
	s.healthMu.Unlock()
	s.mu.Lock()
	// A merged course is usable only when ALL of its codes were fetched.
	// Cache entries remain reusable, so resuming can complete the group.
	good := map[string]bool{}
	for _, row := range j.Rows {
		good[row.Code] = row.Error == ""
	}
	incomplete := map[string]bool{}
	for _, group := range j.codeGroups {
		complete := true
		for _, code := range group {
			if !good[code] {
				complete = false
			}
		}
		if !complete {
			for _, code := range group {
				incomplete[code] = true
			}
		}
	}
	for i := range j.Rows {
		if incomplete[j.Rows[i].Code] && j.Rows[i].Error == "" {
			j.Rows[i].Error = "ข้อมูลรหัสวิชาควบรวมยังไม่ครบ กรุณาดึงต่อหรือตรวจสอบทุกรหัสใน REG ก่อนบันทึก"
		}
	}
	j.Status, j.Error, j.FinishedAt, j.stop = status, msg, &now, nil
	s.busy = false
	s.mu.Unlock()
}

// Stop ends the term's running fetch at once — the request in flight is
// abandoned, nothing more is sent to the registrar. What was fetched so far
// stays in the job (status "stopped") for staff to use, and each fetched
// code's answer stays cached, so starting again only fetches the rest.
func (s *RegEnrolmentService) Stop(ctx context.Context, actor, termID uuid.UUID, selected ...string) (*RegEnrolmentJob, error) {
	source, err := regSource(selected)
	if err != nil {
		return nil, err
	}
	if err := s.mayFetch(ctx, actor); err != nil {
		return nil, err
	}
	s.mu.Lock()
	j := s.jobs[regJobKey{termID, source}]
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
	regErrLayout   = "ข้อมูลจาก REG ไม่ครบ เปลี่ยนระหว่างดึง หรือมีประเภทกลุ่มเรียนที่ยังไม่รองรับ กรุณาตรวจสอบใน REG"
)

// fetchCode uses authoritative level-filtered API results. Sum repeated
// section numbers across revisions only after class-ID deduplication in Fetch.
func (s *RegEnrolmentService) fetchCode(ctx context.Context, termID uuid.UUID, code string, pre []regkku.Section, year, semester int, selected ...string) RegEnrolmentRow {
	source, _ := regSource(selected)
	row := RegEnrolmentRow{Code: code}
	if pre == nil {
		found, err := s.fetchReg(ctx, source, code, year, semester, map[string]bool{code: true})
		if err != nil {
			return regFailure(code, source, err)
		}
		pre = found[code]
	}
	if len(pre) == 0 {
		row.Error = regErrNotFound
		return row
	}
	type key struct {
		no      string
		special bool
	}
	counts := map[key]int{}
	for _, sec := range pre {
		no := strings.TrimLeft(sec.No, "0")
		if no == "" {
			row.Error = regErrLayout
			return row
		}
		counts[key{no, sec.Special}] += sec.Enrolled
	}
	var parts []string
	for k, count := range counts {
		track := "ปกติ"
		if k.special {
			row.Special += count
			track = "พิเศษ"
		} else {
			row.Regular += count
		}
		parts = append(parts, fmt.Sprintf("%s %s %d", k.no, track, count))
		row.SectionCounts = append(row.SectionCounts, SectionCount{SecNo: k.no, Special: k.special, Count: count})
	}
	sort.Strings(parts)
	sort.Slice(row.SectionCounts, func(i, j int) bool {
		a, b := row.SectionCounts[i], row.SectionCounts[j]
		if a.SecNo == b.SecNo {
			return !a.Special && b.Special
		}
		return a.SecNo < b.SecNo
	})
	row.Sections = strings.Join(parts, ", ")
	return row
}

const regWildcardMin = 5

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

// Large groups are fetched once. A failed prefix is NOT retried for every
// code: that would multiply traffic during a REG outage or rate limit.
func (s *RegEnrolmentService) searchGroups(ctx context.Context, termID uuid.UUID, codes []string, year, semester int, selected ...string) (map[string][]regkku.Section, map[string]error) {
	source, _ := regSource(selected)
	groups := map[string][]string{}
	s.mu.Lock()
	for _, code := range codes {
		if cached, ok := s.codes[regCodeKey{term: termID, code: code, year: year, semester: semester, source: source}]; ok && time.Since(cached.at) < regResultTTL {
			continue
		}
		if p := regPattern(code); p != "" {
			groups[p] = append(groups[p], code)
		}
	}
	s.mu.Unlock()
	out := map[string][]regkku.Section{}
	failures := map[string]error{}
	var patterns []string
	for p, list := range groups {
		if len(list) >= regWildcardMin {
			patterns = append(patterns, p)
		}
	}
	sort.Strings(patterns)
	for _, p := range patterns {
		wanted := map[string]bool{}
		for _, code := range groups[p] {
			wanted[code] = true
		}
		found, err := s.fetchReg(ctx, source, p, year, semester, wanted)
		for _, code := range groups[p] {
			if err != nil && len(found[code]) == 0 {
				failures[code] = err
			} else {
				out[code] = found[code]
				if out[code] == nil {
					out[code] = []regkku.Section{}
				}
			}
		}
		if err != nil && !errors.Is(err, regkku.ErrLayout) {
			// Stop prefetching other prefixes after any transport/busy error.
			for _, code := range codes {
				if _, ok := out[code]; !ok {
					failures[code] = err
				}
			}
			break
		}
	}
	return out, failures
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

func regRowsComplete(rows []RegEnrolmentRow) bool {
	for _, r := range rows {
		if r.Error != "" {
			return false
		}
	}
	return true
}

func regSource(selected []string) (string, error) {
	source := regkku.SourceNew
	if len(selected) > 0 && selected[0] != "" {
		source = selected[0]
	}
	if source != regkku.SourceNew && source != regkku.SourceLegacy {
		return "", Invalid("invalid REG source")
	}
	return source, nil
}

func (s *RegEnrolmentService) fetchReg(ctx context.Context, source, pattern string, year, semester int, wanted map[string]bool) (map[string][]regkku.Section, error) {
	if source == regkku.SourceLegacy {
		return s.legacy.Fetch(ctx, pattern, year, semester, wanted)
	}
	return s.reg.Fetch(ctx, pattern, year, semester, wanted)
}

func regFailure(code, source string, err error) RegEnrolmentRow {
	problem := regkku.DescribeAccess(source, err)
	return RegEnrolmentRow{Code: code, Error: regFetchError(code, err), Problem: &problem}
}
