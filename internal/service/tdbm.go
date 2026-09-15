package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TDBMService pulls holidays and lecturer-filed makeup-teaching submissions
// ("สอนชดเชย") from TDBM (tdbm.computing.kku.ac.th) — the college's own,
// and (since 2026-09-15) ONLY, system of record for both. The Bank of
// Thailand holiday sync this replaced (HolidayService.SyncFromBOT, formerly
// here) is gone; see docs/TDBM-API-requirements.md for why.
//
// Extra-teachings rows are landed in the tdbm_extra_teachings staging table,
// NOT auto-filed into makeup_schedules — see migration 0116's header comment.
// TDBM added course_code + owner_teacher_name (2026-09-14) and then
// section + semester_type (2026-09-15, migration 0117), so a row can now be
// resolved down to one specific `sections` row (see resolveCourseMatches,
// resolveSectionMatches). Auto-filing into makeup_schedules still waits on a
// staff review step being built, not on this sync itself.
//
// DESIGN DECISION for whoever builds that auto-fill: when a resolved
// tdbm_extra_teachings row is filed as a makeup, it must NOT be run back
// through our own holiday-overlap validation (loadHolidaysInRange /
// holidaySet in worklog.go, AddMakeup's own check). TDBM's date/time on that
// row IS the record of when the college actually held the makeup class — if
// it happens to fall on what our public_holidays calendar calls a holiday
// (all-day, because TDBM doesn't give us a half-day window — see
// docs/TDBM-API-requirements.md §6.1), that is our calendar being imprecise,
// not the makeup being invalid. Confirmed explicitly: TDBM's own record wins,
// our holiday model does not get a veto over it.
type TDBMService struct {
	pool    *pgxpool.Pool
	apiBase string

	// syncMu/syncing coalesce bursts of webhook pings (or a webhook racing the
	// hourly scheduler tick) into one in-flight sync instead of piling up
	// concurrent pulls against the same term. A ping that arrives while a sync
	// is already running is dropped — the run in flight will see whatever
	// changed, and if it lands mid-write, the next scheduler tick (or the next
	// webhook ping) picks it up within the hour regardless.
	syncMu  sync.Mutex
	syncing bool
}

// ---------------------------------------------------------------------------
// Upstream row shapes — only the fields we consume are declared.
//
// tdbmHolidayRow captured against the live API on 2026-08-22 and unchanged as
// of API_DOCUMENTATION.md (2026-09-14) — still no half-day window, still see
// docs/TDBM-API-requirements.md for the rest of what's still asked for.
//
// tdbmExtraTeachingRow was REPLACED wholesale on 2026-09-14: TDBM added
// course_code + owner_teacher_name (what we asked for) and, per
// API_DOCUMENTATION.md, dropped every other field the old shape had —
// detail, status, teacher_id, holiday_id, teaching_id, class_id, dbm_id,
// etdoc_id, created_user_id, both timestamps. Confirmed against the live
// endpoint the same day; this struct matches what actually comes back, not
// just the doc.
// ---------------------------------------------------------------------------

type tdbmHolidayRow struct {
	HolidayID int    `json:"holiday_id"`
	HDate     string `json:"h_date"`
	Title     string `json:"title"`
	// HType: "E" = compensatory teaching allowed (a real holiday), "D" = —
	// per API_DOCUMENTATION.md — "day off only". That gloss conflicts with
	// what the college itself confirmed when this sync first shipped: 'D' is
	// an EXAM day, not a holiday, and must never block worklog entry or feed
	// the makeup reminder (see SyncHolidays' skip below and migration 0103).
	// Trusting the direct confirmation over the vendor doc's one-line gloss
	// until someone reconciles the two with TDBM directly — don't "fix" the
	// skip below to match this doc without doing that first.
	HType     string `json:"h_type"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type tdbmExtraTeachingRow struct {
	ExtraClassID int     `json:"extra_class_id"`
	Title        *string `json:"title"`
	ClassDate    string  `json:"class_date"`
	StartTime    string  `json:"start_time"`
	EndTime      string  `json:"end_time"`
	Duration     int     `json:"duration"`
	// CourseCode is the real REG subject code (e.g. "CP411105"); see
	// resolveCourseMatches.
	CourseCode *string `json:"course_code"`
	// Section (e.g. "กลุ่มที่ 2") and SemesterType (e.g. "ภาคพิเศษ") arrived
	// 2026-09-15, on top of CourseCode — together they resolve a submission
	// down to one specific `sections` row instead of just the course; see
	// resolveSectionMatches.
	Section      *string `json:"section"`
	SemesterType *string `json:"semester_type"`
	// OwnerTeacherName is resolved upstream via the COURSE's owner_teacher_id,
	// not necessarily whoever actually filed this specific submission (see
	// API_DOCUMENTATION.md's relationship diagram: extra_teaching.class_id →
	// classes.course_id → courses.owner_teacher_id). Close enough to use for
	// display; not guaranteed to be "the lecturer who submitted this request"
	// if a co-lecturer or TA filed on the owner's behalf.
	OwnerTeacherName *string `json:"owner_teacher_name"`
	// OptStatus: P=pending, D=preparing/bundled, A=approved (counted in
	// payments), C=cancelled/rejected (duration=0) — see
	// API_DOCUMENTATION.md's state diagram. No CHECK constraint on the
	// column: better to store a code we don't yet recognize than to fail the
	// whole sync over one.
	OptStatus string `json:"opt_status"`
}

// ---------------------------------------------------------------------------
// Fetch — GET against the public (unauthenticated) TDBM API. Every call needs
// the browser User-Agent below: TDBM's WAF 403s any client that doesn't send
// one (see docs/TDBM-API-requirements.md §4) — asking them to drop this
// requirement is on the same list as the API-key ask.
// ---------------------------------------------------------------------------

const tdbmUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"

func (s *TDBMService) fetchJSON(ctx context.Context, path string, out any) error {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	url := strings.TrimRight(s.apiBase, "/") + path
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, url, nil)
	if err != nil {
		return &UserError{Status: 502, Msg: "สร้างคำขอไปยัง TDBM API ไม่สำเร็จ"}
	}
	req.Header.Set("User-Agent", tdbmUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &UserError{Status: 502, Msg: "ไม่สามารถติดต่อ TDBM API ได้ (network / timeout)"}
	}
	defer resp.Body.Close()

	// 10 MB cap — the largest observed unfiltered payload was ~5.9 MB; a
	// term-scoped pull is far smaller (see docs/TDBM-API-requirements.md §4, §8).
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return &UserError{Status: 502, Msg: "อ่านผลลัพธ์จาก TDBM API ไม่สำเร็จ"}
	}
	if resp.StatusCode != http.StatusOK {
		return &UserError{Status: 502, Msg: fmt.Sprintf("TDBM API ตอบกลับผิดปกติ (%d)", resp.StatusCode)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &UserError{Status: 502, Msg: "รูปแบบข้อมูลจาก TDBM API เปลี่ยน โปรดตรวจสอบ"}
	}
	return nil
}

func (s *TDBMService) fetchHolidays(ctx context.Context, academicYear, semester int) ([]tdbmHolidayRow, error) {
	var rows []tdbmHolidayRow
	path := fmt.Sprintf("/holidays?academic_year=%d&semester=%d", academicYear, semester)
	if err := s.fetchJSON(ctx, path, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *TDBMService) fetchExtraTeachings(ctx context.Context, academicYear, semester int) ([]tdbmExtraTeachingRow, error) {
	var rows []tdbmExtraTeachingRow
	path := fmt.Sprintf("/extra-teachings?academic_year=%d&semester=%d", academicYear, semester)
	if err := s.fetchJSON(ctx, path, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// TDBMSyncResult — shared shape for every sync method below, and the row
// written to tdbm_sync_log.
// ---------------------------------------------------------------------------

type TDBMSyncResult struct {
	Resource     string `json:"resource"`
	AcademicYear int    `json:"academic_year,omitempty"`
	Semester     int    `json:"semester,omitempty"`
	Fetched      int    `json:"fetched"`
	Inserted     int    `json:"inserted"`
	Updated      int    `json:"updated"`
	Skipped      int    `json:"skipped"`
	// Matched — extra-teachings only — is how many of this term's rows now
	// resolve to a teaching_course via course_code; see resolveCourseMatches.
	// Zero-value for every other resource, and not written to the log row for
	// those (see logSync).
	Matched int    `json:"matched,omitempty"`
	Error   string `json:"error,omitempty"`
}

// logSync writes one tdbm_sync_log row for a completed (or failed) resource
// pull. Best-effort: a failure to WRITE the log must never turn a real sync
// result into an error the caller has to handle specially, so it only logs.
func (s *TDBMService) logSync(ctx context.Context, resource, triggerKind string, year, semester int, started time.Time, r TDBMSyncResult, runErr error) {
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
	}
	var yearArg, semArg any
	if year > 0 {
		yearArg, semArg = year, semester
	}
	// matched only means anything for extra-teachings (see TDBMSyncResult.Matched's
	// doc comment) — leave it NULL for every other resource rather than a
	// misleading 0.
	var matchedArg any
	if resource == "extra-teachings" {
		matchedArg = r.Matched
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO tdbm_sync_log (resource, trigger_kind, academic_year, semester, fetched, inserted, updated, skipped, matched, error, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())`,
		resource, triggerKind, yearArg, semArg, r.Fetched, r.Inserted, r.Updated, r.Skipped, matchedArg, nilStrOrEmpty(errMsg), started); err != nil {
		log.Printf("tdbm: failed to write sync log resource=%s err=%v", resource, err)
	}
}

// SyncHolidays pulls TDBM's holiday calendar for one term and upserts it into
// public_holidays (source='tdbm'), keyed by tdbm_holiday_id — see migration
// 0097. Each row is its own statement (not one batch transaction): TDBM
// holidays are also subject to the pre-existing (date, source, window) unique
// index, and two different tdbm_holiday_ids landing on the same date+window
// would abort a shared transaction outright. Per-row isolates that to one
// skipped row instead of losing the whole pull.
//
// start_time/end_time are deliberately left untouched on UPDATE: TDBM sends no
// half-day window at all (see docs/TDBM-API-requirements.md §6.1's request for
// one), so every TDBM-sourced row lands all-day. Staff can still narrow a
// specific one via the existing Patch endpoint, exactly as they do for
// 'custom' holidays today — and a resync must not silently undo that.
func (s *TDBMService) SyncHolidays(ctx context.Context, triggerKind string, academicYear, semester int) TDBMSyncResult {
	started := time.Now()
	res := TDBMSyncResult{Resource: "holidays", AcademicYear: academicYear, Semester: semester}
	rows, err := s.fetchHolidays(ctx, academicYear, semester)
	if err != nil {
		res.Error = err.Error()
		s.logSync(ctx, "holidays", triggerKind, academicYear, semester, started, res, err)
		return res
	}
	res.Fetched = len(rows)

	for _, r := range rows {
		// h_type is undocumented upstream (docs/TDBM-API-requirements.md §3.2),
		// but confirmed with the college: 'D' is an exam day, not a holiday.
		// Exam days must never land in public_holidays — they'd wrongly block
		// worklog entry and count toward the lecturer makeup reminder — and
		// staff already have a separate mandatory manual entry for real
		// holidays, so there is nothing for this sync to reconcile against.
		if r.HType == "D" {
			res.Skipped++
			continue
		}
		if r.HolidayID == 0 || strings.TrimSpace(r.HDate) == "" || strings.TrimSpace(r.Title) == "" {
			res.Skipped++
			continue
		}
		if _, err := time.Parse("2006-01-02", r.HDate); err != nil {
			res.Skipped++
			continue
		}
		note := fmt.Sprintf("นำเข้าจาก TDBM (holiday_id=%d, h_type=%s, status=%s)", r.HolidayID, r.HType, r.Status)
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO public_holidays (holiday_date, name_th, source, note, tdbm_holiday_id)
			VALUES ($1::date, $2, 'tdbm', $3, $4)
			ON CONFLICT (tdbm_holiday_id) DO UPDATE
			   SET holiday_date = EXCLUDED.holiday_date,
			       name_th      = EXCLUDED.name_th`,
			r.HDate, r.Title, note, r.HolidayID)
		if err != nil {
			if isUniqueViolation(err) {
				// Collided with the (date, source, window) index instead — a second
				// tdbm_holiday_id landing all-day on a date TDBM already gave us.
				// Rare (unobserved in the initial pull) but not impossible; skip
				// rather than fail the whole run.
				res.Skipped++
				continue
			}
			res.Error = err.Error()
			s.logSync(ctx, "holidays", triggerKind, academicYear, semester, started, res, err)
			return res
		}
		if tag.RowsAffected() > 0 {
			// pgx can't distinguish INSERT from the UPDATE arm of ON CONFLICT via
			// RowsAffected alone; both count as "inserted" here since neither BOT
			// sync nor this one needs the split for anything operational — the log
			// row's fetched/skipped counts are what staff actually check.
			res.Inserted++
		}
	}
	s.logSync(ctx, "holidays", triggerKind, academicYear, semester, started, res, nil)
	return res
}

// SyncExtraTeachings pulls TDBM's makeup-teaching submissions for one term
// into the tdbm_extra_teachings staging table (see migration 0116 for why
// this does not write to makeup_schedules). One batch transaction is safe
// here — extra_class_id is the only unique key on this table.
func (s *TDBMService) SyncExtraTeachings(ctx context.Context, triggerKind string, academicYear, semester int) TDBMSyncResult {
	started := time.Now()
	res := TDBMSyncResult{Resource: "extra-teachings", AcademicYear: academicYear, Semester: semester}
	rows, err := s.fetchExtraTeachings(ctx, academicYear, semester)
	if err != nil {
		res.Error = err.Error()
		s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
		return res
	}
	res.Fetched = len(rows)
	if len(rows) == 0 {
		s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, nil)
		return res
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		res.Error = err.Error()
		s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
		return res
	}
	defer tx.Rollback(ctx)

	for _, r := range rows {
		if r.ExtraClassID == 0 || strings.TrimSpace(r.ClassDate) == "" {
			res.Skipped++
			continue
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO tdbm_extra_teachings (
				extra_class_id, academic_year, semester, title, class_date, start_time, end_time,
				duration_minutes, course_code, owner_teacher_name, section_label, semester_type, opt_status, synced_at)
			VALUES ($1,$2,$3,$4,$5::date,NULLIF($6,'')::time,NULLIF($7,'')::time,$8,$9,$10,$11,$12,$13,NOW())
			ON CONFLICT (extra_class_id) DO UPDATE
			   SET title = EXCLUDED.title, class_date = EXCLUDED.class_date,
			       start_time = EXCLUDED.start_time, end_time = EXCLUDED.end_time,
			       duration_minutes = EXCLUDED.duration_minutes, course_code = EXCLUDED.course_code,
			       owner_teacher_name = EXCLUDED.owner_teacher_name, section_label = EXCLUDED.section_label,
			       semester_type = EXCLUDED.semester_type, opt_status = EXCLUDED.opt_status,
			       synced_at = NOW()`,
			r.ExtraClassID, academicYear, semester, r.Title, r.ClassDate, r.StartTime, r.EndTime,
			r.Duration, r.CourseCode, r.OwnerTeacherName, r.Section, r.SemesterType, r.OptStatus)
		if err != nil {
			res.Error = err.Error()
			s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
			return res
		}
		if tag.RowsAffected() > 0 {
			res.Inserted++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		res.Error = err.Error()
		s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
		return res
	}

	// Non-fatal below: the raw rows are already committed and usable either
	// way; a failed match pass just means the resolved columns stay whatever
	// they were (possibly stale) until the next sync tries again.
	courseMatched, err := s.resolveCourseMatches(ctx, academicYear, semester)
	if err != nil {
		log.Printf("tdbm: resolve course matches (%d/%d) err=%v", academicYear, semester, err)
	}
	if sectionMatched, err := s.resolveSectionMatches(ctx, academicYear, semester); err != nil {
		log.Printf("tdbm: resolve section matches (%d/%d) err=%v", academicYear, semester, err)
	} else {
		// Matched is the section-level count, not the course-level one: that's
		// the number staff actually care about — "how many of these can we act
		// on right now" — course-only matches still need a human to pick the
		// section, same as no match at all. courseMatched is logged (below)
		// for diagnosing a bad match rate, not persisted.
		res.Matched = sectionMatched
	}
	log.Printf("tdbm: extra-teachings term=%d/%d match course=%d/%d section=%d/%d",
		academicYear, semester, courseMatched, res.Fetched, res.Matched, res.Fetched)

	s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, nil)
	return res
}

// resolveCourseMatches (re)computes teaching_course_id for every
// tdbm_extra_teachings row in one term, matching course_code against
// teaching_courses.code OR .alt_codes — migration 0111 exists because the
// registrar routinely reopens one actual course under a second code, and
// TDBM's course_code could name either. Re-run after every sync (not just
// once) so it self-heals both ways: a course imported into our system AFTER
// TDBM data arrives picks up its match on the next run, and a match that
// stops holding (a code corrected, a row's course_code edited upstream)
// clears back to NULL instead of pointing at a stale course forever — the
// `IS DISTINCT FROM` guard is what makes the second half true for NULLs too.
//
// Returns how many of this term's rows now resolve to a course, for
// TDBMSyncResult.Matched.
func (s *TDBMService) resolveCourseMatches(ctx context.Context, academicYear, semester int) (int, error) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE tdbm_extra_teachings t
		SET teaching_course_id = m.tc_id
		FROM (
			SELECT t2.extra_class_id,
			       (SELECT tc.id FROM teaching_courses tc
			        JOIN academic_terms term ON term.id = tc.term_id
			        WHERE term.academic_year = $1 AND term.semester = $2
			          AND (tc.code = t2.course_code OR t2.course_code = ANY(tc.alt_codes))
			        LIMIT 1) AS tc_id
			FROM tdbm_extra_teachings t2
			WHERE t2.academic_year = $1 AND t2.semester = $2
		) m
		WHERE m.extra_class_id = t.extra_class_id
		  AND t.teaching_course_id IS DISTINCT FROM m.tc_id`,
		academicYear, semester); err != nil {
		return 0, err
	}
	var matched int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM tdbm_extra_teachings
		WHERE academic_year = $1 AND semester = $2 AND teaching_course_id IS NOT NULL`,
		academicYear, semester).Scan(&matched)
	return matched, err
}

// resolveSectionMatches narrows an already course-matched row down to one
// `sections` row, using section_label ("กลุ่มที่ N" -> sections.sec_no) and
// semester_type ("ภาคปกติ"/"ภาคพิเศษ" -> sections.track). Run AFTER
// resolveCourseMatches, and self-heals the same way (including clearing back
// to NULL when teaching_course_id itself just cleared — a row with no course
// match can never have a section match, so the LEFT-joined subquery
// correctly returns NULL for it rather than an earlier match going stale).
//
// sec_no comparison is numeric, not string, to survive zero-padding
// ("01" vs "1") — guarded by a digits-only check on both sides first, because
// migration 0111 lets a section's sec_no carry the alternate registrar code
// too ("SC313302-01"), and casting THAT to ::int would error the whole
// statement rather than just fail to match.
//
// track match is skipped (treated as a pass) when semester_type is an
// unrecognized value — including NULL — rather than refusing the section
// match outright: TDBM might introduce a third semester_type we haven't seen,
// and a name+group match with an unrecognized track label is still stronger
// evidence than no match at all.
func (s *TDBMService) resolveSectionMatches(ctx context.Context, academicYear, semester int) (int, error) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE tdbm_extra_teachings t
		SET section_id = m.sec_id
		FROM (
			SELECT t2.extra_class_id,
			       (SELECT sec.id FROM sections sec
			        WHERE sec.teaching_course_id = t2.teaching_course_id
			          AND (
			                CASE
			                  WHEN sec.sec_no ~ '^\d+$' AND regexp_replace(COALESCE(t2.section_label, ''), '\D', '', 'g') ~ '^\d+$'
			                    THEN sec.sec_no::int = regexp_replace(t2.section_label, '\D', '', 'g')::int
			                  ELSE sec.sec_no = regexp_replace(COALESCE(t2.section_label, ''), '\D', '', 'g')
			                END
			              )
			          AND (
			                t2.semester_type IS NULL
			             OR t2.semester_type NOT IN ('ภาคปกติ', 'ภาคพิเศษ')
			             OR sec.track = (CASE t2.semester_type
			                               WHEN 'ภาคปกติ' THEN 'regular'
			                               WHEN 'ภาคพิเศษ' THEN 'special'
			                             END)::section_track
			          )
			        LIMIT 1) AS sec_id
			FROM tdbm_extra_teachings t2
			WHERE t2.academic_year = $1 AND t2.semester = $2
		) m
		WHERE m.extra_class_id = t.extra_class_id
		  AND t.section_id IS DISTINCT FROM m.sec_id`,
		academicYear, semester); err != nil {
		return 0, err
	}
	var matched int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM tdbm_extra_teachings
		WHERE academic_year = $1 AND semester = $2 AND section_id IS NOT NULL`,
		academicYear, semester).Scan(&matched)
	return matched, err
}

// SyncAll runs holidays and extra-teachings for the currently active academic
// term (academic_terms.is_active — migration 0066 guarantees at most one). A
// failure in one resource does not stop the other: a bad extra-teachings pull
// should not also cost that run's holiday sync.
//
// triggerKind is "webhook" (TDBM POSTed /tdbm-webhook), "scheduler" (hourly
// safety-net sweep — see internal/scheduler), or "manual" (staff clicked the
// button). Purely a label carried into tdbm_sync_log.
func (s *TDBMService) SyncAll(ctx context.Context, triggerKind string) ([]TDBMSyncResult, error) {
	var year, semester int
	err := s.pool.QueryRow(ctx, `SELECT academic_year, semester FROM academic_terms WHERE is_active = true LIMIT 1`).
		Scan(&year, &semester)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, Invalid("ไม่มีภาคเรียนที่เปิดใช้งานอยู่ ข้ามการซิงก์ข้อมูลจาก TDBM")
	}
	if err != nil {
		return nil, err
	}

	results := []TDBMSyncResult{
		s.SyncHolidays(ctx, triggerKind, year, semester),
		s.SyncExtraTeachings(ctx, triggerKind, year, semester),
	}
	for _, r := range results {
		if r.Error != "" {
			log.Printf("tdbm: sync %s failed: %s", r.Resource, r.Error)
		} else {
			log.Printf("tdbm: sync %s ok fetched=%d inserted=%d skipped=%d", r.Resource, r.Fetched, r.Inserted, r.Skipped)
		}
	}
	return results, nil
}

// TriggerAsync kicks off SyncAll in the background and returns immediately —
// what POST /tdbm-webhook calls so it can answer TDBM's request fast instead
// of holding the connection open for however long three upstream pulls take.
// A ping that arrives while a sync is already in flight is dropped; see the
// syncing field's doc comment.
func (s *TDBMService) TriggerAsync(triggerKind string) {
	s.syncMu.Lock()
	if s.syncing {
		s.syncMu.Unlock()
		log.Printf("tdbm: sync already in flight, dropping %s trigger", triggerKind)
		return
	}
	s.syncing = true
	s.syncMu.Unlock()

	go func() {
		defer func() {
			s.syncMu.Lock()
			s.syncing = false
			s.syncMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if _, err := s.SyncAll(ctx, triggerKind); err != nil {
			log.Printf("tdbm: async sync (%s) err=%v", triggerKind, err)
		}
	}()
}

// RecentSyncLog returns the most recent sync log rows, newest first, for the
// staff-facing "last sync" view.
type TDBMSyncLogEntry struct {
	ID           uuid.UUID `json:"id"`
	Resource     string    `json:"resource"`
	TriggerKind  string    `json:"trigger_kind"`
	AcademicYear *int      `json:"academic_year,omitempty"`
	Semester     *int      `json:"semester,omitempty"`
	Fetched      int       `json:"fetched"`
	Inserted     int       `json:"inserted"`
	Updated      int       `json:"updated"`
	Skipped      int       `json:"skipped"`
	// Matched is only meaningful for resource="extra-teachings" — see
	// TDBMSyncResult.Matched. NULL (omitted) for every other resource.
	Matched    *int    `json:"matched,omitempty"`
	Error      *string `json:"error,omitempty"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at,omitempty"`
}

func (s *TDBMService) RecentSyncLog(ctx context.Context, limit int) ([]TDBMSyncLogEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, resource, trigger_kind, academic_year, semester, fetched, inserted, updated, skipped, matched, error,
		       TO_CHAR(started_at, 'YYYY-MM-DD"T"HH24:MI:SS'),
		       TO_CHAR(finished_at, 'YYYY-MM-DD"T"HH24:MI:SS')
		FROM tdbm_sync_log
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TDBMSyncLogEntry{}
	for rows.Next() {
		var e TDBMSyncLogEntry
		var finished *string
		if err := rows.Scan(&e.ID, &e.Resource, &e.TriggerKind, &e.AcademicYear, &e.Semester,
			&e.Fetched, &e.Inserted, &e.Updated, &e.Skipped, &e.Matched, &e.Error, &e.StartedAt, &finished); err != nil {
			return nil, err
		}
		e.FinishedAt = finished
		out = append(out, e)
	}
	return out, rows.Err()
}
