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
// ("สอนชดเชย") from TDBM (tdbm.computing.kku.ac.th) — the college's own
// system of record for both, per docs/TDBM-API-requirements.md. It is the
// TDBM analogue of HolidayService.SyncFromBOT, but for two upstream resources
// instead of one, plus a teachers mirror needed to make sense of the second.
//
// Extra-teachings rows are landed in the tdbm_extra_teachings staging table,
// NOT auto-filed into makeup_schedules — see migration 0097's header comment
// for why: TDBM's feed carries no subject code, so there is nothing reliable
// to match against our own sections with yet.
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
// Upstream row shapes — only the fields we consume are declared. Captured
// against the live API on 2026-08-22; see docs/TDBM-API-requirements.md for
// what we've since asked TDBM to change (envelope, pagination, gzip,
// updated_since, a subject code on extra-teachings, etc).
// ---------------------------------------------------------------------------

type tdbmHolidayRow struct {
	HolidayID int    `json:"holiday_id"`
	HDate     string `json:"h_date"`
	Title     string `json:"title"`
	HType     string `json:"h_type"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type tdbmExtraTeachingRow struct {
	ExtraClassID int     `json:"extra_class_id"`
	Title        *string `json:"title"`
	Detail       *string `json:"detail"`
	OptStatus    string  `json:"opt_status"`
	Status       string  `json:"status"`
	ClassDate    string  `json:"class_date"`
	StartTime    string  `json:"start_time"`
	EndTime      string  `json:"end_time"`
	Duration     int     `json:"duration"`
	TeacherID    int     `json:"teacher_id"`
	HolidayID    *int    `json:"holiday_id"`
	TeachingID   *int    `json:"teaching_id"`
	ClassID      *int    `json:"class_id"`
	DBMID        *int    `json:"dbm_id"`
	EtdocID      *int    `json:"etdoc_id"`
	CreatedUser  int     `json:"created_user_id"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

type tdbmTeacherRow struct {
	TeacherID     int     `json:"teacher_id"`
	Prefix        string  `json:"prefix"`
	Position      string  `json:"position"`
	Degree        string  `json:"degree"`
	Name          string  `json:"name"`
	Email         string  `json:"email"`
	AccountUserID *int    `json:"account_user_id"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
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

func (s *TDBMService) fetchTeachers(ctx context.Context) ([]tdbmTeacherRow, error) {
	var rows []tdbmTeacherRow
	if err := s.fetchJSON(ctx, "/teachers", &rows); err != nil {
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
	Error        string `json:"error,omitempty"`
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
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO tdbm_sync_log (resource, trigger_kind, academic_year, semester, fetched, inserted, updated, skipped, error, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())`,
		resource, triggerKind, yearArg, semArg, r.Fetched, r.Inserted, r.Updated, r.Skipped, nilStrOrEmpty(errMsg), started); err != nil {
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

// SyncTeachers refreshes the tdbm_teachers mirror in full — small enough
// (~60 rows) that a plain per-row upsert needs no pagination or diffing.
func (s *TDBMService) SyncTeachers(ctx context.Context, triggerKind string) TDBMSyncResult {
	started := time.Now()
	res := TDBMSyncResult{Resource: "teachers"}
	rows, err := s.fetchTeachers(ctx)
	if err != nil {
		res.Error = err.Error()
		s.logSync(ctx, "teachers", triggerKind, 0, 0, started, res, err)
		return res
	}
	res.Fetched = len(rows)

	for _, r := range rows {
		if r.TeacherID == 0 || strings.TrimSpace(r.Name) == "" {
			res.Skipped++
			continue
		}
		_, err := s.pool.Exec(ctx, `
			INSERT INTO tdbm_teachers (teacher_id, prefix, "position", degree, name, email, account_user_id, synced_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
			ON CONFLICT (teacher_id) DO UPDATE
			   SET prefix = EXCLUDED.prefix, "position" = EXCLUDED."position", degree = EXCLUDED.degree,
			       name = EXCLUDED.name, email = EXCLUDED.email, account_user_id = EXCLUDED.account_user_id,
			       synced_at = NOW()`,
			r.TeacherID, nilStrOrEmpty(r.Prefix), nilStrOrEmpty(r.Position), nilStrOrEmpty(r.Degree),
			r.Name, nilStrOrEmpty(r.Email), r.AccountUserID)
		if err != nil {
			res.Error = err.Error()
			s.logSync(ctx, "teachers", triggerKind, 0, 0, started, res, err)
			return res
		}
		res.Inserted++
	}
	s.logSync(ctx, "teachers", triggerKind, 0, 0, started, res, nil)
	return res
}

// SyncExtraTeachings pulls TDBM's makeup-teaching submissions for one term
// into the tdbm_extra_teachings staging table (see migration 0097 for why
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
				extra_class_id, academic_year, semester, title, detail, opt_status, status,
				class_date, start_time, end_time, duration_minutes, teacher_id, holiday_id,
				teaching_id, class_id, dbm_id, etdoc_id, created_user_id, synced_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::date,NULLIF($9,'')::time,NULLIF($10,'')::time,$11,$12,$13,$14,$15,$16,$17,$18,NOW())
			ON CONFLICT (extra_class_id) DO UPDATE
			   SET title = EXCLUDED.title, detail = EXCLUDED.detail,
			       opt_status = EXCLUDED.opt_status, status = EXCLUDED.status,
			       class_date = EXCLUDED.class_date, start_time = EXCLUDED.start_time, end_time = EXCLUDED.end_time,
			       duration_minutes = EXCLUDED.duration_minutes, teacher_id = EXCLUDED.teacher_id,
			       holiday_id = EXCLUDED.holiday_id, teaching_id = EXCLUDED.teaching_id, class_id = EXCLUDED.class_id,
			       dbm_id = EXCLUDED.dbm_id, etdoc_id = EXCLUDED.etdoc_id, synced_at = NOW()`,
			r.ExtraClassID, academicYear, semester, nilStrOrEmpty(strPtrOrEmpty(r.Title)), nilStrOrEmpty(strPtrOrEmpty(r.Detail)),
			nilStrOrEmpty(r.OptStatus), nilStrOrEmpty(r.Status), r.ClassDate, r.StartTime, r.EndTime,
			r.Duration, r.TeacherID, r.HolidayID, r.TeachingID, r.ClassID, r.DBMID, r.EtdocID, r.CreatedUser)
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
	s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, nil)
	return res
}

func strPtrOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// SyncAll runs teachers → holidays → extra-teachings for the currently active
// academic term (academic_terms.is_active — migration 0066 guarantees at most
// one). A failure in one resource does not stop the others, same partial-
// success philosophy as SyncFromBOTRange: a bad extra-teachings pull should
// not also cost that run's holiday sync.
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
		s.SyncTeachers(ctx, triggerKind),
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
	Error        *string   `json:"error,omitempty"`
	StartedAt    string    `json:"started_at"`
	FinishedAt   *string   `json:"finished_at,omitempty"`
}

func (s *TDBMService) RecentSyncLog(ctx context.Context, limit int) ([]TDBMSyncLogEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, resource, trigger_kind, academic_year, semester, fetched, inserted, updated, skipped, error,
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
			&e.Fetched, &e.Inserted, &e.Updated, &e.Skipped, &e.Error, &e.StartedAt, &finished); err != nil {
			return nil, err
		}
		e.FinishedAt = finished
		out = append(out, e)
	}
	return out, nil
}
