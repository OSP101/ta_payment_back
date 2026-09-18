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

	"ta-payment-back/internal/audit"
)

// TDBMService pulls holidays and lecturer-filed makeup-teaching submissions
// ("สอนชดเชย") from TDBM (tdbm.computing.kku.ac.th) — the college's own,
// and (since 2026-09-15) ONLY, system of record for both. The Bank of
// Thailand holiday sync this replaced (HolidayService.SyncFromBOT, formerly
// here) is gone; see docs/TDBM-API-requirements.md for why.
//
// Extra-teachings rows are landed in the tdbm_extra_teachings staging table
// AND — since 2026-09-18, see AutoFillMakeupSchedules — auto-filed into
// makeup_schedules once resolved down to a section. TDBM added course_code +
// owner_teacher_name (2026-09-14) and then section + semester_type
// (2026-09-15, migration 0117) to get there (see resolveCourseMatches,
// resolveSectionMatches). Decided explicitly (2026-09-18): lecturers are
// already required to file in TDBM, so its record is trusted as-is rather
// than held for a staff confirmation step — see AutoFillMakeupSchedules'
// own doc comment for the real limitation this runs into (TDBM does not say
// WHICH holiday a submission replaces) and how it's worked around.
//
// DESIGN DECISION, already implemented by AutoFillMakeupSchedules: an
// auto-filed makeup is never run back through our own holiday-overlap
// validation (loadHolidaysInRange / holidaySet in worklog.go, AddMakeup's own
// check) — it writes directly via pool.Exec, not through AddMakeup. TDBM's
// date/time on a row IS the record of when the college actually held the
// makeup class — if it happens to fall on what our public_holidays calendar
// calls a holiday (all-day, because TDBM doesn't give us a half-day window —
// see docs/TDBM-API-requirements.md §6.1), that is our calendar being
// imprecise, not the makeup being invalid. TDBM's own record wins.
type TDBMService struct {
	pool    *pgxpool.Pool
	aud     *audit.Auditor
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

	if filled, err := s.AutoFillMakeupSchedules(ctx, academicYear, semester); err != nil {
		log.Printf("tdbm: auto-fill makeup schedules (%d/%d) err=%v", academicYear, semester, err)
	} else if filled > 0 {
		log.Printf("tdbm: auto-filled %d makeup_schedules row(s) from TDBM (%d/%d)", filled, academicYear, semester)
	}

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

// ResolveMatchesForTerm re-runs course/section matching for one term right
// now, instead of waiting for the next TDBM sync (webhook ping, or the hourly
// scheduler sweep otherwise). Call this whenever OUR OWN teaching_courses or
// sections change for a term that already has TDBM data sitting unmatched —
// TDBM syncing has no way to know that happened, since nothing on TDBM's side
// changed. Both passes are idempotent, so calling this on every course/section
// write is safe even when there is nothing to fix.
//
// Errors are logged, not returned: this runs as a side effect of a course/
// section write that has already succeeded, and a failed re-match must not
// turn that success into a request error — the next sync sweep (within the
// hour) still catches it.
func (s *TDBMService) ResolveMatchesForTerm(ctx context.Context, academicYear, semester int) {
	if _, err := s.resolveCourseMatches(ctx, academicYear, semester); err != nil {
		log.Printf("tdbm: resolve course matches after course/section change (%d/%d) err=%v", academicYear, semester, err)
		return
	}
	if _, err := s.resolveSectionMatches(ctx, academicYear, semester); err != nil {
		log.Printf("tdbm: resolve section matches after course/section change (%d/%d) err=%v", academicYear, semester, err)
		return
	}
	if filled, err := s.AutoFillMakeupSchedules(ctx, academicYear, semester); err != nil {
		log.Printf("tdbm: auto-fill makeup schedules after course/section change (%d/%d) err=%v", academicYear, semester, err)
	} else if filled > 0 {
		log.Printf("tdbm: auto-filled %d makeup_schedules row(s) after course/section change (%d/%d)", filled, academicYear, semester)
	}
}

// ResolveMatchesForTermID is ResolveMatchesForTerm for a caller that only has
// the term's id (e.g. CommitImport) — looks up academic_year/semester first.
func (s *TDBMService) ResolveMatchesForTermID(ctx context.Context, termID uuid.UUID) {
	var year, semester int
	if err := s.pool.QueryRow(ctx,
		`SELECT academic_year, semester FROM academic_terms WHERE id = $1`, termID).
		Scan(&year, &semester); err != nil {
		log.Printf("tdbm: resolve matches — look up term %s err=%v", termID, err)
		return
	}
	s.ResolveMatchesForTerm(ctx, year, semester)
}

// ResolveMatchesForTeachingCourse is ResolveMatchesForTerm for a caller that
// only has a teaching_courses id (e.g. AddSection) — looks up its term first.
func (s *TDBMService) ResolveMatchesForTeachingCourse(ctx context.Context, tcID uuid.UUID) {
	var year, semester int
	err := s.pool.QueryRow(ctx, `
		SELECT term.academic_year, term.semester
		FROM teaching_courses tc
		JOIN academic_terms term ON term.id = tc.term_id
		WHERE tc.id = $1`, tcID).Scan(&year, &semester)
	if err != nil {
		log.Printf("tdbm: resolve matches — look up course %s err=%v", tcID, err)
		return
	}
	s.ResolveMatchesForTerm(ctx, year, semester)
}

// ---------------------------------------------------------------------------
// AutoFillMakeupSchedules — file section-matched TDBM rows as real makeups.
// ---------------------------------------------------------------------------

// unresolvedPeriod is one (section, holiday, kind) that ImpactsForCourse would
// list as "ยังไม่ได้กำหนดวันชดเชย" — no makeup_schedules row exists yet.
type unresolvedPeriod struct {
	SectionID   uuid.UUID
	HolidayDate string // "YYYY-MM-DD"
	Kind        string // "lecture" | "lab"
}

// tdbmCandidate is one section-matched, not-yet-applied tdbm_extra_teachings
// row — a real submission whose ORIGINAL holiday is unknown to us (see the
// package doc comment); only its proposed makeup date/time and section.
type tdbmCandidate struct {
	ExtraClassID int
	SectionID    uuid.UUID
	ClassDate    string
	StartTime    string
	EndTime      string
}

// holidayGroup is one section's still-open holiday date and the (one or two)
// periods it still needs a makeup for — see AutoFillMakeupSchedules.
type holidayGroup struct {
	date  string
	kinds []unresolvedPeriod
}

// tdbmCluster is one section's still-unapplied TDBM submissions sharing a
// single class_date — see AutoFillMakeupSchedules.
type tdbmCluster struct {
	classDate string
	entries   []tdbmCandidate
}

// scheduleDuration resolves the ORIGINAL scheduled duration for a (section,
// kind), used to disambiguate which TDBM entry is which kind when a matched
// pair has two entries — see pairAndApply.
func (s *TDBMService) scheduleDuration(ctx context.Context, sectionID uuid.UUID, kind string) int {
	var mins int
	_ = s.pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM (end_time - start_time))::int / 60
		FROM section_schedules WHERE section_id = $1 AND kind = $2 LIMIT 1`,
		sectionID, kind).Scan(&mins)
	return mins
}

func entryDurationMins(c tdbmCandidate) int {
	st, ok1 := parseHM(c.StartTime)
	et, ok2 := parseHM(c.EndTime)
	if !ok1 || !ok2 {
		return 0
	}
	return et - st
}

// AutoFillMakeupSchedules pairs each section's still-unresolved holiday
// periods against that section's still-unapplied TDBM submissions and writes
// makeup_schedules directly (bypassing AddMakeup's validation on purpose —
// see the package doc comment's DESIGN DECISION). Returns how many rows it
// created, for the caller's log line.
//
// THE CORE PROBLEM: TDBM's /extra-teachings no longer says which holiday a
// submission replaces (dropped along with teacher_id/holiday_id/class_id when
// course_code arrived — see migration 0116). All it gives per section is a
// proposed makeup date/time. So this can only INFER the pairing:
//
//  1. Group a section's unresolved (holiday_date, kind) rows by date, in
//     date order — each group is "what this ONE holiday still needs" (one or
//     two periods; a section normally teaches at most lecture+lab per day).
//  2. Group that section's unapplied TDBM rows by class_date, in date order —
//     lecturers who file a makeup normally file the lecture+lab pair for one
//     holiday under the same target class_date, so this is the TDBM-side
//     analogue of a "holiday group".
//  3. First, pair any TDBM group whose class_date is EXACTLY one of the
//     section's still-open holiday dates straight to that holiday — this
//     covers lecturers who hold the makeup class ON the holiday itself
//     (a normal thing to do: the holiday is exactly when the room/slot is
//     free) and is a hard fact, not a guess, so it always wins.
//  4. Whatever holidays and TDBM groups are left after step 3, pair group i
//     with group i, in date order — the oldest still-open holiday gets the
//     oldest still-unapplied TDBM submission for that section.
//
// SAFETY RAILS, because #4 is a real guess that a lecturer filing out of
// chronological order would break (step 3 needs no such rail — an exact
// date match is certain):
//   - A pair is used ONLY when both groups have exactly the same number of
//     entries (both 1, or both 2) — a count mismatch means the guess has
//     nothing solid to anchor on, so that pair is skipped and logged rather
//     than forced. It stays "ยังไม่ได้กำหนดวันชดเชย" until either side's
//     count changes (a later sync, or a manual entry) makes it resolvable.
//   - Within a matched pair of size 2, kind is assigned by matching duration
//     against each kind's OWN scheduled duration when they differ; when they
//     are equal (as they often are — two 2-hour blocks), earliest scheduled
//     kind is paired with the earliest TDBM start_time. Still a guess, which
//     is why every auto-filled row's note names the exact extra_class_id it
//     came from — traceable and correctable by hand if the guess was wrong.
//   - ON CONFLICT DO NOTHING on the insert, and applied_makeup_id (migration
//     0118) marks a TDBM row consumed the moment it succeeds, so re-running
//     this on every sync can only fill NEW gaps, never re-pair or duplicate
//     an already-resolved one.
//   - Only pulls candidates with opt_status <> 'C' (cancelled/rejected) —
//     P/D/A all count, per this project's standing policy of using a
//     submission as soon as it exists rather than waiting for full approval.
//   - Skips any course already exported (assertNotExported's own column) —
//     that term's payout is locked; a new makeup there could not change
//     anything a TA can still log against.
func (s *TDBMService) AutoFillMakeupSchedules(ctx context.Context, academicYear, semester int) (int, error) {
	unresolved, err := s.loadUnresolvedPeriods(ctx, academicYear, semester)
	if err != nil {
		return 0, err
	}
	if len(unresolved) == 0 {
		return 0, nil
	}
	candidates, err := s.loadTDBMCandidates(ctx, academicYear, semester)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	bySectionHolidays := map[uuid.UUID][]holidayGroup{}
	for _, u := range unresolved {
		gs := bySectionHolidays[u.SectionID]
		if n := len(gs); n > 0 && gs[n-1].date == u.HolidayDate {
			gs[n-1].kinds = append(gs[n-1].kinds, u)
		} else {
			gs = append(gs, holidayGroup{date: u.HolidayDate, kinds: []unresolvedPeriod{u}})
		}
		bySectionHolidays[u.SectionID] = gs
	}

	bySectionClusters := map[uuid.UUID][]tdbmCluster{}
	for _, c := range candidates {
		cs := bySectionClusters[c.SectionID]
		if n := len(cs); n > 0 && cs[n-1].classDate == c.ClassDate {
			cs[n-1].entries = append(cs[n-1].entries, c)
		} else {
			cs = append(cs, tdbmCluster{classDate: c.ClassDate, entries: []tdbmCandidate{c}})
		}
		bySectionClusters[c.SectionID] = cs
	}

	filled := 0
	for sectionID, groups := range bySectionHolidays {
		clusters := bySectionClusters[sectionID]

		// Step 3: exact class_date == holiday_date matches first — certain,
		// not a guess, so they're pulled out before the chronological
		// fallback below ever sees them.
		var remainingGroups []holidayGroup
		usedClusterIdx := map[int]bool{}
		for _, g := range groups {
			matched := false
			for ci, cl := range clusters {
				if usedClusterIdx[ci] || cl.classDate != g.date {
					continue
				}
				usedClusterIdx[ci] = true
				matched = true
				filled += s.pairAndApply(ctx, sectionID, g, cl)
				break
			}
			if !matched {
				remainingGroups = append(remainingGroups, g)
			}
		}
		var remainingClusters []tdbmCluster
		for ci, cl := range clusters {
			if !usedClusterIdx[ci] {
				remainingClusters = append(remainingClusters, cl)
			}
		}

		// Step 4: whatever is left, pair chronologically by index as before.
		n := len(remainingGroups)
		if len(remainingClusters) < n {
			n = len(remainingClusters)
		}
		for i := 0; i < n; i++ {
			g, cl := remainingGroups[i], remainingClusters[i]
			filled += s.pairAndApply(ctx, sectionID, g, cl)
		}
	}
	return filled, nil
}

// pairAndApply applies one matched (holidayGroup, cluster) pair — shared by
// AutoFillMakeupSchedules' exact-date-match pass and its chronological
// fallback pass. Returns how many rows it created (0 or 1 on a count
// mismatch or insert error, both already logged).
func (s *TDBMService) pairAndApply(ctx context.Context, sectionID uuid.UUID, g holidayGroup, cl tdbmCluster) int {
	if len(g.kinds) != len(cl.entries) {
		log.Printf("tdbm: auto-fill skip section=%s holiday=%s — %d period(s) needed vs %d TDBM entr(y/ies) on %s (count mismatch, left for manual entry)",
			sectionID, g.date, len(g.kinds), len(cl.entries), cl.classDate)
		return 0
	}
	// Assign kind↔entry. len is 1 or 2 in every real case (a section
	// holds at most lecture+lab on one weekday); anything else falls
	// through to the trivial index-order pairing below, which is
	// exactly right for len==1 and a best-effort for a len we've
	// never actually observed.
	order := make([]int, len(cl.entries))
	for idx := range order {
		order[idx] = idx
	}
	if len(g.kinds) == 2 && len(cl.entries) == 2 {
		d0 := s.scheduleDuration(ctx, sectionID, g.kinds[0].Kind)
		d1 := s.scheduleDuration(ctx, sectionID, g.kinds[1].Kind)
		e0, e1 := entryDurationMins(cl.entries[0]), entryDurationMins(cl.entries[1])
		if d0 != d1 && d0 > 0 && d1 > 0 {
			// Durations differ — match each kind to the entry whose
			// length actually equals it, don't assume any order.
			if d0 == e1 && d1 == e0 {
				order = []int{1, 0}
			}
			// else already [0,1]: either it matches, or neither
			// duration lines up (data is just noisy) and index order
			// is the only thing left to fall back on.
		}
		// Equal (or unknown) durations: cl.entries and g.kinds are
		// both already ordered by start_time ascending (see the two
		// loader queries), so index order already pairs "earliest
		// scheduled kind" with "earliest TDBM start_time".
	}
	filled := 0
	for j, kindNeeded := range g.kinds {
		entry := cl.entries[order[j]]
		n, err := s.applyOneMakeup(ctx, sectionID, kindNeeded, entry)
		if err != nil {
			log.Printf("tdbm: auto-fill insert err section=%s date=%s kind=%s extra_class_id=%d err=%v",
				sectionID, kindNeeded.HolidayDate, kindNeeded.Kind, entry.ExtraClassID, err)
			continue
		}
		filled += n
	}
	return filled
}

// applyOneMakeup inserts one makeup_schedules row from one TDBM candidate and
// marks that candidate consumed, both non-transactionally (a crash between
// the two leaves the makeup filed and the TDBM row eligible to be picked up
// again — resolveSectionMatches + this function's ON CONFLICT DO NOTHING mean
// the retry is a safe no-op, not a duplicate). Returns 1 on a genuine new
// insert, 0 if the slot was already filled by something else (a manual entry,
// or a previous run that didn't get to mark this row applied).
func (s *TDBMService) applyOneMakeup(ctx context.Context, sectionID uuid.UUID, kindNeeded unresolvedPeriod, entry tdbmCandidate) (int, error) {
	makeupID := uuid.New()
	note := fmt.Sprintf("นำเข้าอัตโนมัติจาก TDBM (extra_class_id=%d)", entry.ExtraClassID)
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, start_time, end_time, note, kind)
		VALUES ($1,$2,$3::date,$4::date,$5,$6,$7,$8)
		ON CONFLICT (section_id, original_date, kind) DO NOTHING`,
		makeupID, sectionID, kindNeeded.HolidayDate, entry.ClassDate, entry.StartTime, entry.EndTime, note, kindNeeded.Kind)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, nil
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE tdbm_extra_teachings SET applied_makeup_id = $1 WHERE extra_class_id = $2`,
		makeupID, entry.ExtraClassID); err != nil {
		return 1, err
	}
	if err := s.aud.Log(ctx, audit.Entry{
		Action: "makeup.auto_fill_from_tdbm", Entity: "section", EntityID: sectionID.String(),
		Note: fmt.Sprintf("original=%s kind=%s makeup=%s %s-%s (extra_class_id=%d)",
			kindNeeded.HolidayDate, kindNeeded.Kind, entry.ClassDate, entry.StartTime, entry.EndTime, entry.ExtraClassID),
	}); err != nil {
		log.Printf("tdbm: auto-fill audit log err extra_class_id=%d err=%v", entry.ExtraClassID, err)
	}
	return 1, nil
}

// loadUnresolvedPeriods is UnresolvedMakeupsSQL (course_window.go) widened
// from one course to a whole term, returning rows instead of a count, and
// excluding exported courses. The join must stay identical to
// UnresolvedMakeupsSQL / HolidayService.ImpactsForCourse — see that
// function's own header comment on why three copies already drifted once.
func (s *TDBMService) loadUnresolvedPeriods(ctx context.Context, academicYear, semester int) ([]unresolvedPeriod, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sx.id, TO_CHAR(h.holiday_date,'YYYY-MM-DD'), sch.kind
		FROM teaching_courses tc
		JOIN academic_terms term ON term.id = tc.term_id
		JOIN sections sx ON sx.teaching_course_id = tc.id
		JOIN public_holidays h ON h.holiday_date BETWEEN `+CourseStartSQL("tc")+` AND `+CourseEndSQL("tc")+`
		JOIN section_schedules sch ON sch.section_id = sx.id
		     AND sch.day_of_week = EXTRACT(DOW FROM h.holiday_date)::int
		     AND (h.start_time IS NULL OR (sch.start_time < h.end_time AND sch.end_time > h.start_time))
		LEFT JOIN makeup_schedules m ON m.section_id = sx.id
		     AND m.original_date = h.holiday_date AND m.kind = sch.kind
		WHERE term.academic_year = $1 AND term.semester = $2
		  AND tc.exported_at IS NULL
		  AND m.id IS NULL
		ORDER BY sx.id, h.holiday_date, sch.start_time`,
		academicYear, semester)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []unresolvedPeriod{}
	for rows.Next() {
		var u unresolvedPeriod
		if err := rows.Scan(&u.SectionID, &u.HolidayDate, &u.Kind); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// loadTDBMCandidates lists section-matched TDBM rows not yet consumed by a
// previous auto-fill, ordered so entries sharing (section, class_date) come
// out grouped and start_time-ascending within the group — see
// AutoFillMakeupSchedules' pairing algorithm.
func (s *TDBMService) loadTDBMCandidates(ctx context.Context, academicYear, semester int) ([]tdbmCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT extra_class_id, section_id, TO_CHAR(class_date,'YYYY-MM-DD'),
		       TO_CHAR(start_time,'HH24:MI'), TO_CHAR(end_time,'HH24:MI')
		FROM tdbm_extra_teachings
		WHERE academic_year = $1 AND semester = $2
		  AND section_id IS NOT NULL
		  AND applied_makeup_id IS NULL
		  AND opt_status <> 'C'
		ORDER BY section_id, class_date, start_time`,
		academicYear, semester)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []tdbmCandidate{}
	for rows.Next() {
		var c tdbmCandidate
		if err := rows.Scan(&c.ExtraClassID, &c.SectionID, &c.ClassDate, &c.StartTime, &c.EndTime); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
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
	year, semester, err := s.activeTerm(ctx)
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

// activeTerm resolves the currently active academic term — the scope every
// sync method runs against (see SyncAll). Factored out because ListExtraTeachings
// needs the same "no active term" fallback when the caller doesn't specify one.
func (s *TDBMService) activeTerm(ctx context.Context) (year, semester int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT academic_year, semester FROM academic_terms WHERE is_active = true LIMIT 1`).
		Scan(&year, &semester)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, Invalid("ไม่มีภาคเรียนที่เปิดใช้งานอยู่")
	}
	return year, semester, err
}

// TDBMExtraTeaching is one row for the staff-facing "ข้อมูลจาก TDBM" view —
// GET /tdbm/extra-teachings. It exists because tdbm_extra_teachings itself
// carries only what TDBM sent (course_code, section_label, …) plus opaque
// foreign keys (teaching_course_id, section_id); this joins those keys back
// to human-readable names so staff can see AT A GLANCE that the sync found
// real data and where each row landed — matched to an exact section, matched
// to a course only (section_label didn't resolve), or not matched at all.
type TDBMExtraTeaching struct {
	ExtraClassID     int     `json:"extra_class_id"`
	ClassDate        string  `json:"class_date"`
	StartTime        *string `json:"start_time,omitempty"`
	EndTime          *string `json:"end_time,omitempty"`
	DurationMinutes  int     `json:"duration_minutes"`
	CourseCode       *string `json:"course_code,omitempty"`
	OwnerTeacherName *string `json:"owner_teacher_name,omitempty"`
	SectionLabel     *string `json:"section_label,omitempty"`
	SemesterType     *string `json:"semester_type,omitempty"`
	OptStatus        string  `json:"opt_status"`
	// Matched* are populated only when resolveCourseMatches / resolveSectionMatches
	// found a row in OUR OWN data — never trust CourseCode/SectionLabel above as
	// already-verified against our schema, they are TDBM's raw text.
	MatchedCourseCode *string `json:"matched_course_code,omitempty"`
	MatchedCourseName *string `json:"matched_course_name,omitempty"`
	MatchedSecNo      *string `json:"matched_sec_no,omitempty"`
	// OriginalHolidayDate/Name come from the makeup_schedules row this entry
	// filled (applied_makeup_id, migration 0118) — the ANSWER to "which
	// holiday is this compensating for", which TDBM itself never says (see
	// AutoFillMakeupSchedules' doc comment). Both nil when this entry hasn't
	// been auto-filled yet (no clean 1:1 pairing found, or still pending).
	OriginalHolidayDate *string `json:"original_holiday_date,omitempty"`
	OriginalHolidayName *string `json:"original_holiday_name,omitempty"`
	SyncedAt            string  `json:"synced_at"`
}

// ListExtraTeachings returns every tdbm_extra_teachings row for one term,
// newest class_date first, with match info joined in. academicYear==0 uses
// the active term (same resolution as SyncAll).
func (s *TDBMService) ListExtraTeachings(ctx context.Context, academicYear, semester int) ([]TDBMExtraTeaching, error) {
	if academicYear == 0 {
		var err error
		academicYear, semester, err = s.activeTerm(ctx)
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.extra_class_id, TO_CHAR(t.class_date,'YYYY-MM-DD'),
		       TO_CHAR(t.start_time,'HH24:MI'), TO_CHAR(t.end_time,'HH24:MI'),
		       t.duration_minutes, t.course_code, t.owner_teacher_name,
		       t.section_label, t.semester_type, t.opt_status,
		       tc.code, tc.name_th, sec.sec_no,
		       TO_CHAR(m.original_date,'YYYY-MM-DD'), h.name_th,
		       TO_CHAR(t.synced_at, 'YYYY-MM-DD"T"HH24:MI:SS')
		FROM tdbm_extra_teachings t
		LEFT JOIN teaching_courses tc ON tc.id = t.teaching_course_id
		LEFT JOIN sections sec ON sec.id = t.section_id
		LEFT JOIN makeup_schedules m ON m.id = t.applied_makeup_id
		-- Best-effort name for the original date: two closures on one date
		-- (an all-day one plus a faculty half-day) would pick either — a
		-- display nicety, not the join that decides original_date itself.
		LEFT JOIN LATERAL (
			SELECT name_th FROM public_holidays WHERE holiday_date = m.original_date LIMIT 1
		) h ON m.id IS NOT NULL
		WHERE t.academic_year = $1 AND t.semester = $2
		ORDER BY t.class_date DESC, t.extra_class_id DESC`,
		academicYear, semester)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TDBMExtraTeaching{}
	for rows.Next() {
		var e TDBMExtraTeaching
		if err := rows.Scan(&e.ExtraClassID, &e.ClassDate, &e.StartTime, &e.EndTime,
			&e.DurationMinutes, &e.CourseCode, &e.OwnerTeacherName,
			&e.SectionLabel, &e.SemesterType, &e.OptStatus,
			&e.MatchedCourseCode, &e.MatchedCourseName, &e.MatchedSecNo,
			&e.OriginalHolidayDate, &e.OriginalHolidayName, &e.SyncedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
