package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// TDBMService mirrors holidays and makeup sittings from the college's MariaDB.
// Webhooks are wake-up signals only. Every successful pull is a full term
// snapshot, including cancelled and deleted rows. There is no HTTP fallback.
type TDBMService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	source tdbmSource

	// Coalesce a burst into an in-flight run and one trailing run.
	syncMu  sync.Mutex
	syncing bool
	pending bool
}

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
	ExtraClassID     int     `json:"extra_class_id"`
	Title            *string `json:"title"`
	ClassDate        string  `json:"class_date"`
	StartTime        string  `json:"start_time"`
	EndTime          string  `json:"end_time"`
	Duration         int     `json:"duration"`
	CourseCode       *string `json:"course_code"`
	Section          *string `json:"section"`
	SemesterType     *string `json:"semester_type"`
	OwnerTeacherName *string `json:"owner_teacher_name"`
	TeachingID       *int    `json:"source_teaching_id"`
	ClassID          *int    `json:"source_class_id"`
	HolidayID        *int
	OriginalDate     *string `json:"source_original_date"`
	OriginalStart    *string `json:"source_original_start"`
	OriginalEnd      *string `json:"source_original_end"`
	OriginalKind     *string
	LatestStatusID   *int
	SourceUpdatedAt  *string
	StatusUpdatedAt  *string
	DocumentID       *int `json:"source_document_id"`
	SourceDuration   *int `json:"source_duration_minutes"`
	Reference        bool `json:"source_reference"`
	// Only A and D are eligible; every status is mirrored for reconciliation.
	OptStatus string `json:"opt_status"`
}

func tdbmMakeupEligible(status string) bool {
	return status == "A" || status == "D"
}

// ---------------------------------------------------------------------------
// TDBMSyncResult — shared shape for every sync method below, and the row
// written to tdbm_sync_log.
// ---------------------------------------------------------------------------

const tdbmSyncFailureMessage = "ซิงก์ข้อมูลจาก TDBM ไม่สำเร็จ กรุณาลองใหม่ภายหลังหรือติดต่อผู้ดูแลระบบ"

// This exact application-generated warning contains only a count and dates.
// Do not accept arbitrary Thai text: legacy rows can contain internal errors.
var tdbmSyncBlockedPattern = regexp.MustCompile(`\Aข้ามวันหยุดจาก TDBM [0-9]+ วัน \([0-9]{4}-[0-9]{2}-[0-9]{2}(, [0-9]{4}-[0-9]{2}-[0-9]{2})*\) เพราะมีบันทึกเวลาคาบบรรยาย/ปฏิบัติการที่ส่งหรืออนุมัติแล้วในวันนั้น ให้อาจารย์ตีกลับรายการเหล่านั้นก่อน แล้วซิงก์อีกครั้ง\z`)

var tdbmSyncWarningPattern = regexp.MustCompile(`\Aซิงก์ข้อมูล TDBM แล้ว แต่มี [0-9]+ รายการที่ต้องตรวจสอบคาบชดเชย\z`)

// Only application-owned messages may leave the sync service. This
// also protects historical log rows containing raw SQL, URLs or credentials.
func publicTDBMSyncError(msg string) string {
	if msg == "" || msg == tdbmReconcileConflict || tdbmSyncBlockedPattern.MatchString(msg) || tdbmSyncWarningPattern.MatchString(msg) {
		return msg
	}
	return tdbmSyncFailureMessage
}

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
	Matched   int    `json:"matched,omitempty"`
	Error     string `json:"error,omitempty"`
	Warning   string `json:"warning,omitempty"`
	Conflicts int    `json:"conflicts,omitempty"`
}

// logSync writes one tdbm_sync_log row for a completed (or failed) resource
// pull. Best-effort: a failure to WRITE the log must never turn a real sync
// result into an error the caller has to handle specially, so it only logs.
func (s *TDBMService) logSync(ctx context.Context, resource, triggerKind string, year, semester int, started time.Time, r TDBMSyncResult, runErr error) {
	errMsg := ""
	if runErr != nil {
		log.Printf("tdbm: sync failed resource=%s trigger=%s err=%v", resource, triggerKind, runErr)
		errMsg = publicTDBMSyncError(r.Error)
		if errMsg == "" {
			errMsg = tdbmSyncFailureMessage
		}
	} else if tdbmSyncWarningPattern.MatchString(r.Warning) {
		// Keep compatibility with existing sync history storage. The read
		// boundary exposes this application-owned message as a warning.
		errMsg = r.Warning
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
	snapshot, err := s.snapshot(ctx, academicYear, semester)
	if err != nil {
		res.Error = tdbmSyncFailureMessage
		s.logSync(ctx, "holidays", triggerKind, academicYear, semester, started, res, err)
		return res
	}
	return s.syncHolidayRows(ctx, triggerKind, academicYear, semester, snapshot)
}

func (s *TDBMService) syncHolidayRows(ctx context.Context, triggerKind string, academicYear, semester int, snapshot tdbmSnapshot) TDBMSyncResult {
	started := time.Now()
	rows := snapshot.Holidays
	res := TDBMSyncResult{Resource: "holidays", AcademicYear: academicYear, Semester: semester, Fetched: len(rows)}
	var blocked []string
	activeHolidayIDs := make([]int, 0, len(rows))
	for _, r := range rows {
		// h_type is undocumented upstream (docs/TDBM-API-requirements.md §3.2),
		// but confirmed with the college: 'D' is an exam day, not a holiday.
		// Exam days must never land in public_holidays — they'd wrongly block
		// worklog entry and count toward the lecturer makeup reminder — and
		// staff already have a separate mandatory manual entry for real
		// holidays, so there is nothing for this sync to reconcile against.
		if r.HType != "E" || r.Status != "A" {
			res.Skipped++
			continue
		}
		// A malformed but still-present active holiday is not a deletion.
		if r.HolidayID > 0 {
			activeHolidayIDs = append(activeHolidayIDs, r.HolidayID)
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
		// A closure that is new (or moved to a new date) cancels class sittings
		// exactly like one staff add by hand, so it goes through the same
		// clearHolidayClasses: unsent lecture/lab drafts on that day are removed,
		// and sent/approved ones refuse the closure. Without this a TDBM holiday
		// left the cancelled class billable next to its makeup. A holiday we
		// already hold on the same date is only renamed — re-checking it every
		// hour would re-raise the same warning forever.
		var curDate *string
		if err := s.pool.QueryRow(ctx,
			`SELECT holiday_date::text FROM public_holidays WHERE tdbm_holiday_id = $1`, r.HolidayID).Scan(&curDate); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			res.Error = tdbmSyncFailureMessage
			s.logSync(ctx, "holidays", triggerKind, academicYear, semester, started, res, err)
			return res
		}
		newClosure := curDate == nil || *curDate != r.HDate
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			res.Error = tdbmSyncFailureMessage
			s.logSync(ctx, "holidays", triggerKind, academicYear, semester, started, res, err)
			return res
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO public_holidays (holiday_date, name_th, source, note, tdbm_holiday_id)
			VALUES ($1::date, $2, 'tdbm', $3, $4)
			ON CONFLICT (tdbm_holiday_id) DO UPDATE
			   SET holiday_date = EXCLUDED.holiday_date,
			       name_th      = EXCLUDED.name_th`,
			r.HDate, r.Title, note, r.HolidayID)
		if err == nil && newClosure {
			err = clearHolidayClasses(ctx, tx, r.HDate, nil, nil)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			if isUniqueViolation(err) {
				// Collided with the (date, source, window) index instead — a second
				// tdbm_holiday_id landing all-day on a date TDBM already gave us.
				// Rare (unobserved in the initial pull) but not impossible; skip
				// rather than fail the whole run.
				res.Skipped++
				continue
			}
			var ue *UserError
			if errors.As(err, &ue) {
				// Sent/approved sittings on the day: not ours to delete. Skip the
				// holiday and say so in the sync log, where staff look.
				res.Skipped++
				blocked = append(blocked, r.HDate)
				continue
			}
			res.Error = tdbmSyncFailureMessage
			s.logSync(ctx, "holidays", triggerKind, academicYear, semester, started, res, err)
			return res
		}
		if err := tx.Commit(ctx); err != nil {
			res.Error = tdbmSyncFailureMessage
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
	if snapshot.StartDate != "" && snapshot.EndDate != "" {
		if err := s.removeAbsentHolidays(ctx, snapshot.StartDate, snapshot.EndDate, activeHolidayIDs); err != nil {
			res.Error = tdbmReconcileConflict
			s.logSync(ctx, "holidays", triggerKind, academicYear, semester, started, res, err)
			return res
		}
	}
	if len(blocked) > 0 {
		warn := fmt.Errorf("ข้ามวันหยุดจาก TDBM %d วัน (%s) เพราะมีบันทึกเวลาคาบบรรยาย/ปฏิบัติการที่ส่งหรืออนุมัติแล้วในวันนั้น ให้อาจารย์ตีกลับรายการเหล่านั้นก่อน แล้วซิงก์อีกครั้ง",
			len(blocked), strings.Join(blocked, ", "))
		res.Error = publicTDBMSyncError(warn.Error())
		s.logSync(ctx, "holidays", triggerKind, academicYear, semester, started, res, warn)
		return res
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
	snapshot, err := s.snapshot(ctx, academicYear, semester)
	if err != nil {
		res.Error = tdbmSyncFailureMessage
		s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
		return res
	}
	return s.syncExtraRows(ctx, triggerKind, academicYear, semester, snapshot.Extra)
}

func (s *TDBMService) syncExtraRows(ctx context.Context, triggerKind string, academicYear, semester int, rows []tdbmExtraTeachingRow) TDBMSyncResult {
	rows = tdbmNormalizeReferences(rows)
	started := time.Now()
	res := TDBMSyncResult{Resource: "extra-teachings", AcademicYear: academicYear, Semester: semester, Fetched: len(rows)}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		res.Error = tdbmSyncFailureMessage
		s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
		return res
	}
	defer tx.Rollback(ctx)

	seen := make([]int, 0, len(rows))
	for _, r := range rows {
		if r.ExtraClassID <= 0 {
			res.Error = tdbmSyncFailureMessage
			s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, errors.New("invalid upstream identity"))
			return res
		}
		seen = append(seen, r.ExtraClassID)
		var inserted bool
		err := tx.QueryRow(ctx, `
		 INSERT INTO tdbm_extra_teachings (extra_class_id,academic_year,semester,title,class_date,start_time,end_time,
		 duration_minutes,course_code,owner_teacher_name,section_label,semester_type,opt_status,
		 source_teaching_id,source_class_id,source_holiday_id,source_original_date,source_original_start,source_original_end,
		 source_original_kind,source_latest_status_id,source_updated_at,source_status_updated_at,source_missing,synced_at,source_document_id,source_duration_minutes,source_reference)
		 VALUES ($1,$2,$3,$4,NULLIF($5,'')::date,NULLIF($6,'')::time,NULLIF($7,'')::time,$8,$9,$10,$11,$12,$13,
		 $14,$15,$16,$17::date,$18::time,$19::time,$20,$21,$22::timestamptz,$23::timestamptz,FALSE,now(),$24,$25,$26)
		 ON CONFLICT(extra_class_id) DO UPDATE SET academic_year=EXCLUDED.academic_year,semester=EXCLUDED.semester,
		 title=EXCLUDED.title,class_date=EXCLUDED.class_date,start_time=EXCLUDED.start_time,end_time=EXCLUDED.end_time,
		 duration_minutes=EXCLUDED.duration_minutes,course_code=EXCLUDED.course_code,owner_teacher_name=EXCLUDED.owner_teacher_name,
		 section_label=EXCLUDED.section_label,semester_type=EXCLUDED.semester_type,opt_status=EXCLUDED.opt_status,
		 source_teaching_id=EXCLUDED.source_teaching_id,source_class_id=EXCLUDED.source_class_id,source_holiday_id=EXCLUDED.source_holiday_id,
		 source_original_date=EXCLUDED.source_original_date,source_original_start=EXCLUDED.source_original_start,
		 source_original_end=EXCLUDED.source_original_end,source_original_kind=EXCLUDED.source_original_kind,
		 source_latest_status_id=EXCLUDED.source_latest_status_id,source_updated_at=EXCLUDED.source_updated_at,
		 source_status_updated_at=EXCLUDED.source_status_updated_at,source_missing=FALSE,synced_at=now(),
		 source_document_id=EXCLUDED.source_document_id,source_duration_minutes=EXCLUDED.source_duration_minutes,source_reference=EXCLUDED.source_reference
		 RETURNING (xmax=0)`, r.ExtraClassID, academicYear, semester, r.Title, r.ClassDate, r.StartTime, r.EndTime, r.Duration,
			r.CourseCode, r.OwnerTeacherName, r.Section, r.SemesterType, r.OptStatus, r.TeachingID, r.ClassID, r.HolidayID,
			r.OriginalDate, r.OriginalStart, r.OriginalEnd, r.OriginalKind, r.LatestStatusID, r.SourceUpdatedAt, r.StatusUpdatedAt, r.DocumentID, r.SourceDuration, r.Reference).Scan(&inserted)
		if err != nil {
			res.Error = tdbmSyncFailureMessage
			s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
			return res
		}
		if inserted {
			res.Inserted++
		} else {
			res.Updated++
		}
	}
	// Only a successfully read, complete snapshot may mark absent rows missing.
	if _, err := tx.Exec(ctx, `UPDATE tdbm_extra_teachings SET source_missing=TRUE,synced_at=now()
	 WHERE academic_year=$1 AND semester=$2 AND NOT(extra_class_id=ANY($3))`, academicYear, semester, seen); err != nil {
		res.Error = tdbmSyncFailureMessage
		s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
		return res
	}
	if err := tx.Commit(ctx); err != nil {
		res.Error = tdbmSyncFailureMessage
		s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
		return res
	}

	// Non-fatal below: the raw rows are already committed and usable either
	// way; a failed match pass just means the resolved columns stay whatever
	// they were (possibly stale) until the next sync tries again.
	courseMatched, err := s.resolveCourseMatches(ctx, academicYear, semester)
	if err != nil {
		res.Error = tdbmSyncFailureMessage
		log.Printf("tdbm: resolve course matches (%d/%d) err=%v", academicYear, semester, err)
		s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
		return res
	}
	if sectionMatched, err := s.resolveSectionMatches(ctx, academicYear, semester); err != nil {
		res.Error = tdbmSyncFailureMessage
		log.Printf("tdbm: resolve section matches (%d/%d) err=%v", academicYear, semester, err)
		s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, err)
		return res
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
		res.Error = tdbmSyncFailureMessage
		log.Printf("tdbm: auto-fill makeup schedules (%d/%d) err=%v", academicYear, semester, err)
	} else if filled > 0 {
		log.Printf("tdbm: auto-filled %d makeup_schedules row(s) from TDBM (%d/%d)", filled, academicYear, semester)
	}

	var conflicts int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tdbm_extra_teachings WHERE academic_year=$1 AND semester=$2 AND reconciliation_error IS NOT NULL`, academicYear, semester).Scan(&conflicts); err != nil {
		res.Error = tdbmSyncFailureMessage
	} else if conflicts > 0 {
		res.Conflicts = conflicts
		res.Warning = fmt.Sprintf("ซิงก์ข้อมูล TDBM แล้ว แต่มี %d รายการที่ต้องตรวจสอบคาบชดเชย", conflicts)
	}
	var runErr error
	if res.Error != "" {
		runErr = errors.New(res.Error)
	}
	s.logSync(ctx, "extra-teachings", triggerKind, academicYear, semester, started, res, runErr)
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
//
// Track is a TIE-BREAKER, not a gate (03/10/2026). CP423324 2569/1: TDBM sent
// "กลุ่มที่ 2 / ภาคพิเศษ" while our only group 2 is stored as regular, so
// every one of that group's makeups stayed unmatched and never reached the
// course's holiday page. The group number already names the section when the
// course has only one group with it; track decides only between two groups
// sharing a number (a regular 01 and a special 01), and is still preferred.
func (s *TDBMService) resolveSectionMatches(ctx context.Context, academicYear, semester int) (int, error) {
	secNoMatches := func(alias string) string {
		return `CASE
		          WHEN ` + alias + `.sec_no ~ '^\d+$' AND regexp_replace(COALESCE(t2.section_label, ''), '\D', '', 'g') ~ '^\d+$'
		            THEN ` + alias + `.sec_no::int = regexp_replace(t2.section_label, '\D', '', 'g')::int
		          ELSE ` + alias + `.sec_no = regexp_replace(COALESCE(t2.section_label, ''), '\D', '', 'g')
		        END`
	}
	trackOK := `(t2.semester_type IS NULL
	             OR t2.semester_type NOT IN ('ภาคปกติ', 'ภาคพิเศษ')
	             OR sec.track = (CASE t2.semester_type
	                               WHEN 'ภาคปกติ' THEN 'regular'
	                               WHEN 'ภาคพิเศษ' THEN 'special'
	                             END)::section_track)`
	if _, err := s.pool.Exec(ctx, `
		UPDATE tdbm_extra_teachings t
		SET section_id = m.sec_id
		FROM (
			SELECT t2.extra_class_id,
			       COALESCE((`+existingMergeTDBMSectionMatch+`), (SELECT sec.id FROM sections sec
			        WHERE sec.teaching_course_id = t2.teaching_course_id
			          AND (`+secNoMatches("sec")+`)
			          AND (`+trackOK+`
			               OR NOT EXISTS (
			                   SELECT 1 FROM sections s2
			                   WHERE s2.teaching_course_id = sec.teaching_course_id
			                     AND s2.id <> sec.id
			                     AND (`+secNoMatches("s2")+`)))
			        ORDER BY `+trackOK+` DESC
			        LIMIT 1)) AS sec_id
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

// AutoFillMakeupSchedules reconciles exact MariaDB original sittings first.
// Legacy staged rows without source metadata retain the previous conservative
// review behavior until their first authoritative MariaDB snapshot arrives.
func (s *TDBMService) AutoFillMakeupSchedules(ctx context.Context, academicYear, semester int) (int, error) {
	directFilled, err := s.reconcileMakeups(ctx, academicYear, semester)
	if err != nil {
		return 0, err
	}
	// A twin of an already-filed sitting (the same makeup filed under the
	// course's other code — see loadTDBMCandidates) points at that makeup
	// too, so the TDBM page stops listing it as unmatched.
	if _, err := s.pool.Exec(ctx, `
		UPDATE tdbm_extra_teachings t SET applied_makeup_id = f.applied_makeup_id
		FROM tdbm_extra_teachings f
		WHERE t.academic_year = $1 AND t.semester = $2
		  AND t.applied_makeup_id IS NULL AND t.opt_status IN ('A','D')
		  AND NOT t.source_missing AND NOT t.source_reference AND t.source_teaching_id IS NULL AND t.source_class_id IS NULL AND t.reconciliation_error IS NULL
		  AND f.applied_makeup_id IS NOT NULL
		  AND f.opt_status IN ('A','D') AND NOT f.source_missing AND NOT f.source_reference AND f.reconciliation_error IS NULL
		  AND f.section_id = t.section_id AND f.class_date = t.class_date
		  AND f.start_time IS NOT DISTINCT FROM t.start_time
		  AND f.end_time IS NOT DISTINCT FROM t.end_time`, academicYear, semester); err != nil {
		return 0, err
	}
	unresolved, err := s.loadUnresolvedPeriods(ctx, academicYear, semester)
	if err != nil {
		return 0, err
	}
	if len(unresolved) == 0 {
		return directFilled, nil
	}
	candidates, err := s.loadTDBMCandidates(ctx, academicYear, semester)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return directFilled, nil
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

		if len(groups) != 1 || len(clusters) != 1 {
			continue
		}
		filled += s.pairAndApply(ctx, sectionID, groups[0], clusters[0])
	}
	return directFilled + filled, nil
}

// pairAndApply applies the only remaining holiday/date group pair.
// A count mismatch remains available for explicit per-period staff review.
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

// applyOneMakeup shares the section lock and transactional writer with staff
// review. Re-check consumed rows and sitting status after acquiring the lock.
func (s *TDBMService) applyOneMakeup(ctx context.Context, sectionID uuid.UUID, kindNeeded unresolvedPeriod, entry tdbmCandidate) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM sections WHERE id = $1 FOR UPDATE`, sectionID).Scan(&locked); err != nil {
		return 0, err
	}
	var taught bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_logs wl
 JOIN ta_request_assignments a ON a.id = wl.assignment_id
 WHERE a.section_id = $1 AND wl.work_date = $2::date AND wl.activity = $3
 AND wl.status IN ('submitted','approved'))`, sectionID, kindNeeded.HolidayDate, kindNeeded.Kind).Scan(&taught); err != nil {
		return 0, err
	}
	if taught {
		return 0, nil
	}
	// A sync may have cancelled or re-matched the entry since candidates loaded.
	var stale bool
	if err := tx.QueryRow(ctx, `SELECT (source_missing OR source_reference OR reconciliation_error IS NOT NULL OR COALESCE(opt_status,'') NOT IN ('A','D') OR applied_makeup_id IS NOT NULL
 OR section_id IS DISTINCT FROM $2 OR class_date IS DISTINCT FROM $3::date
 OR start_time IS DISTINCT FROM $4::time OR end_time IS DISTINCT FROM $5::time)
 FROM tdbm_extra_teachings WHERE extra_class_id = $1 FOR UPDATE`,
		entry.ExtraClassID, sectionID, entry.ClassDate, entry.StartTime, entry.EndTime).Scan(&stale); errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	if stale {
		return 0, nil
	}
	n, err := s.insertTDBMMakeup(ctx, tx, sectionID, kindNeeded, entry, nil)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

// loadUnresolvedPeriods is UnresolvedMakeupsSQL (course_window.go) widened
// from one course to a whole term, returning rows instead of a count, and
// excluding exported courses. The join must stay identical to
// UnresolvedMakeupsSQL / HolidayService.ImpactsForCourse — see that
// function's own header comment on why three copies already drifted once.
func (s *TDBMService) loadUnresolvedPeriods(ctx context.Context, academicYear, semester int) ([]unresolvedPeriod, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT sx.id, TO_CHAR(h.holiday_date,'YYYY-MM-DD'), sch.kind, TO_CHAR(sch.start_time,'HH24:MI')
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
		ORDER BY 1, 2, 4`,
		academicYear, semester)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []unresolvedPeriod{}
	for rows.Next() {
		var u unresolvedPeriod
		if err := rows.Scan(&u.SectionID, &u.HolidayDate, &u.Kind, new(string)); err != nil {
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
//
// One sitting, one candidate (04/10/2026). A merged course is filed in TDBM
// under each of its codes — CP352201 and its alternate SC362201 both carry
// the same group-1 makeup — and both rows resolve to the one section we keep
// (0145). Counted twice, every holiday needing one period met a cluster of
// two and was skipped as a count mismatch, so no makeup of a merged course
// was ever filed. Twins collapse to the lowest extra_class_id here, a twin
// whose sitting is already filed is not a candidate at all (it would pair
// with the NEXT holiday), and applyOneMakeup marks every twin consumed.
func (s *TDBMService) loadTDBMCandidates(ctx context.Context, academicYear, semester int) ([]tdbmCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT extra_class_id, section_id, class_date, start_time, end_time FROM (
			SELECT DISTINCT ON (t.section_id, t.class_date, t.start_time, t.end_time)
			       t.extra_class_id, t.section_id, TO_CHAR(t.class_date,'YYYY-MM-DD') AS class_date,
			       TO_CHAR(t.start_time,'HH24:MI') AS start_time, TO_CHAR(t.end_time,'HH24:MI') AS end_time,
			       t.class_date AS d, t.start_time AS st
			FROM tdbm_extra_teachings t
			WHERE t.academic_year = $1 AND t.semester = $2
			  AND t.section_id IS NOT NULL
			  AND t.applied_makeup_id IS NULL
			  AND t.opt_status IN ('A','D') AND NOT t.source_missing AND NOT t.source_reference
			  AND t.source_teaching_id IS NULL AND t.source_class_id IS NULL AND t.reconciliation_error IS NULL
			  AND NOT EXISTS (
			      SELECT 1 FROM tdbm_extra_teachings tw
			      WHERE tw.section_id = t.section_id AND tw.class_date = t.class_date
			        AND tw.start_time IS NOT DISTINCT FROM t.start_time
			        AND tw.end_time IS NOT DISTINCT FROM t.end_time
			        AND tw.applied_makeup_id IS NOT NULL)
			ORDER BY t.section_id, t.class_date, t.start_time, t.end_time, t.extra_class_id
		) c
		ORDER BY section_id, d, st`,
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

// SyncAll reads each locally used term once. The advisory transaction lock
// serializes manual, scheduled and webhook syncs across backend processes.
func (s *TDBMService) SyncAll(ctx context.Context, triggerKind string) ([]TDBMSyncResult, error) {
	guard, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer guard.Rollback(context.Background())
	var acquired bool
	if err = guard.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(736264219)`).Scan(&acquired); err != nil {
		return nil, err
	}
	if !acquired {
		return nil, Conflict("กำลังซิงก์ข้อมูล TDBM กรุณารอสักครู่")
	}
	rows, err := s.pool.Query(ctx, `SELECT academic_year,semester FROM academic_terms trm
	 WHERE is_active OR EXISTS(SELECT 1 FROM teaching_courses tc WHERE tc.term_id=trm.id)
	 OR EXISTS(SELECT 1 FROM tdbm_extra_teachings t WHERE t.academic_year=trm.academic_year AND t.semester=trm.semester)
	 ORDER BY academic_year DESC,semester DESC`)
	if err != nil {
		return nil, err
	}
	type term struct{ year, semester int }
	var terms []term
	for rows.Next() {
		var t term
		if err = rows.Scan(&t.year, &t.semester); err != nil {
			rows.Close()
			return nil, err
		}
		terms = append(terms, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var results []TDBMSyncResult
	for _, t := range terms {
		started := time.Now()
		snapshot, err := s.snapshot(ctx, t.year, t.semester)
		if err != nil {
			for _, resource := range []string{"holidays", "extra-teachings"} {
				r := TDBMSyncResult{Resource: resource, AcademicYear: t.year, Semester: t.semester, Error: tdbmSyncFailureMessage}
				s.logSync(ctx, resource, triggerKind, t.year, t.semester, started, r, err)
				results = append(results, r)
			}
			continue
		}
		results = append(results, s.syncHolidayRows(ctx, triggerKind, t.year, t.semester, snapshot), s.syncExtraRows(ctx, triggerKind, t.year, t.semester, snapshot.Extra))
	}
	return results, nil
}

// TriggerAsync returns immediately and coalesces bursts into a trailing pull,
// so a change committed during the current snapshot is not dropped.
func (s *TDBMService) TriggerAsync(triggerKind string) {
	s.syncMu.Lock()
	if s.syncing {
		s.pending = true
		s.syncMu.Unlock()
		return
	}
	s.syncing = true
	s.syncMu.Unlock()
	if !backgroundJobs.start(func(parent context.Context) {
		for {
			ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
			_, err := s.SyncAll(ctx, triggerKind)
			cancel()
			if err != nil {
				log.Printf("tdbm: async sync (%s) failed", triggerKind)
			}
			s.syncMu.Lock()
			if s.pending && parent.Err() == nil {
				s.pending = false
				s.syncMu.Unlock()
				continue
			}
			s.syncing = false
			s.pending = false
			s.syncMu.Unlock()
			return
		}
	}) {
		s.syncMu.Lock()
		s.syncing = false
		s.pending = false
		s.syncMu.Unlock()
		log.Printf("tdbm: background worker capacity unavailable")
	}
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
	Warning    *string `json:"warning,omitempty"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at,omitempty"`
}

func (s *TDBMService) RecentSyncLog(ctx context.Context, limit int, term ...int) ([]TDBMSyncLogEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	year, semester := 0, 0
	if len(term) == 2 {
		year, semester = term[0], term[1]
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, resource, trigger_kind, academic_year, semester, fetched, inserted, updated, skipped, matched, error,
		       TO_CHAR(started_at, 'YYYY-MM-DD"T"HH24:MI:SS'),
		       TO_CHAR(finished_at, 'YYYY-MM-DD"T"HH24:MI:SS')
		FROM tdbm_sync_log
		WHERE ($2::int=0 OR (academic_year=$2 AND semester=$3))
		ORDER BY created_at DESC
		LIMIT $1`, limit, year, semester)
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
		// Old rows may contain raw driver errors from before error sanitization.
		if e.Error != nil {
			msg := publicTDBMSyncError(*e.Error)
			if tdbmSyncWarningPattern.MatchString(msg) || msg == tdbmReconcileConflict {
				e.Warning = &msg
				e.Error = nil
			} else {
				e.Error = &msg
			}
		}
		e.FinishedAt = finished
		out = append(out, e)
	}
	return out, rows.Err()
}

// activeTerm supplies the default scope for staff list requests.
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
	ExtraClassID        int     `json:"extra_class_id"`
	ClassDate           string  `json:"class_date"`
	StartTime           *string `json:"start_time,omitempty"`
	EndTime             *string `json:"end_time,omitempty"`
	DurationMinutes     int     `json:"duration_minutes"`
	CourseCode          *string `json:"course_code,omitempty"`
	OwnerTeacherName    *string `json:"owner_teacher_name,omitempty"`
	SectionLabel        *string `json:"section_label,omitempty"`
	SemesterType        *string `json:"semester_type,omitempty"`
	OptStatus           string  `json:"opt_status"`
	SourceMissing       bool    `json:"source_missing"`
	SourceReference     bool    `json:"source_reference"`
	SourceOriginalDate  *string `json:"source_original_date,omitempty"`
	SourceOriginalKind  *string `json:"source_original_kind,omitempty"`
	ReconciliationError *string `json:"reconciliation_error,omitempty"`
	AppliedMakeupDate   *string `json:"applied_makeup_date,omitempty"`
	AppliedStartTime    *string `json:"applied_start_time,omitempty"`
	AppliedEndTime      *string `json:"applied_end_time,omitempty"`
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
	OriginalHolidayDate *string           `json:"original_holiday_date,omitempty"`
	OriginalHolidayName *string           `json:"original_holiday_name,omitempty"`
	MatchedSectionID    *uuid.UUID        `json:"matched_section_id,omitempty"`
	OriginalKind        *string           `json:"original_kind,omitempty"`
	MatchMethod         string            `json:"match_method"`
	ResolutionReason    string            `json:"resolution_reason"`
	MatchOptions        []TDBMMatchOption `json:"match_options"`
	courseExported      bool
	SyncedAt            string `json:"synced_at"`
}

// ListExtraTeachings returns every tdbm_extra_teachings row for one term,
// newest class_date first, with match info joined in. academicYear==0 uses
// the active term.
func (s *TDBMService) ListExtraTeachings(ctx context.Context, academicYear, semester int) ([]TDBMExtraTeaching, error) {
	if academicYear == 0 {
		var err error
		academicYear, semester, err = s.activeTerm(ctx)
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.extra_class_id, COALESCE(TO_CHAR(t.class_date,'YYYY-MM-DD'),''),
		       TO_CHAR(t.start_time,'HH24:MI'), TO_CHAR(t.end_time,'HH24:MI'),
		       t.duration_minutes, t.course_code, t.owner_teacher_name,
		       t.section_label, t.semester_type, COALESCE(t.opt_status,''),t.source_missing,t.source_reference,
		       t.source_original_date::text,t.source_original_kind,t.reconciliation_error,
		       to_char(m.makeup_date,'YYYY-MM-DD'),to_char(m.start_time,'HH24:MI'),to_char(m.end_time,'HH24:MI'),
		       tc.code, tc.name_th, sec.sec_no,
		       TO_CHAR(m.original_date,'YYYY-MM-DD'), h.name_th, sec.id, m.kind,
 CASE WHEN m.note LIKE 'เจ้าหน้าที่จับคู่จาก TDBM%' THEN 'manual'
      WHEN m.id IS NOT NULL THEN 'automatic' ELSE '' END, tc.exported_at IS NOT NULL,
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
			&e.SectionLabel, &e.SemesterType, &e.OptStatus, &e.SourceMissing, &e.SourceReference, &e.SourceOriginalDate, &e.SourceOriginalKind, &e.ReconciliationError,
			&e.AppliedMakeupDate, &e.AppliedStartTime, &e.AppliedEndTime,
			&e.MatchedCourseCode, &e.MatchedCourseName, &e.MatchedSecNo,
			&e.OriginalHolidayDate, &e.OriginalHolidayName, &e.MatchedSectionID, &e.OriginalKind, &e.MatchMethod, &e.courseExported, &e.SyncedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	options, err := tdbmMatchOptions(ctx, s.pool, academicYear, semester)
	if err != nil {
		return nil, err
	}
	pendingSittings := map[uuid.UUID]map[string]bool{}
	pendingDates := map[uuid.UUID]map[string]bool{}
	for _, e := range out {
		if e.MatchedSectionID == nil || e.OriginalHolidayDate != nil || e.SourceMissing || e.SourceReference || e.ReconciliationError != nil || !tdbmMakeupEligible(e.OptStatus) {
			continue
		}
		sec := *e.MatchedSectionID
		if pendingSittings[sec] == nil {
			pendingSittings[sec] = map[string]bool{}
			pendingDates[sec] = map[string]bool{}
		}
		st, et := "", ""
		if e.StartTime != nil {
			st = *e.StartTime
		}
		if e.EndTime != nil {
			et = *e.EndTime
		}
		pendingSittings[sec][e.ClassDate+"/"+st+"/"+et] = true
		pendingDates[sec][e.ClassDate] = true
	}
	for i := range out {
		e := &out[i]
		e.MatchOptions = []TDBMMatchOption{}
		switch {
		case e.SourceReference:
			e.ResolutionReason = "รายการอ้างอิงหรือรายการเดิมที่ถูกแทนที่ ใช้คาบปัจจุบันจากเอกสาร TDBM แล้ว"
		case e.ReconciliationError != nil:
			e.ResolutionReason = *e.ReconciliationError
		case e.SourceMissing:
			e.ResolutionReason = "ไม่พบรายการนี้ในฐานข้อมูล TDBM แล้ว"
		case e.OptStatus == "C":
			e.ResolutionReason = "รายการถูกยกเลิกใน TDBM"
		case !tdbmMakeupEligible(e.OptStatus):
			e.ResolutionReason = "ใช้รายการสอนชดเชยจาก TDBM เฉพาะสถานะ A และ D เท่านั้น"
		case e.OriginalHolidayDate != nil:
			continue
		case e.MatchedSectionID == nil:
			e.ResolutionReason = "ยังจับคู่วิชาและกลุ่มเรียนไม่ได้ กรุณาตรวจสอบข้อมูลวิชาและกลุ่ม"
		case e.courseExported:
			e.ResolutionReason = "รายวิชาถูกล็อกหลังส่งออกเอกสารแล้ว"
		case e.StartTime == nil || e.EndTime == nil || *e.StartTime >= *e.EndTime:
			e.ResolutionReason = "ข้อมูลช่วงเวลาสอนจาก TDBM ไม่ครบหรือไม่ถูกต้อง"
		case e.SourceOriginalDate != nil:
			e.ResolutionReason = "รอซิงก์คาบเดิมที่ระบุในฐานข้อมูล TDBM"
		default:
			e.MatchOptions = options[*e.MatchedSectionID]
			if e.MatchOptions == nil {
				e.MatchOptions = []TDBMMatchOption{}
			}
			available := 0
			for _, o := range e.MatchOptions {
				if !o.Blocked {
					available++
				}
			}
			if len(e.MatchOptions) == 0 {
				e.ResolutionReason = "ไม่พบคาบวันหยุดที่ยังว่าง ตรวจสอบตารางเรียน ช่วงวันที่รายวิชา หรือวันชดเชยที่กำหนดไว้แล้ว"
			} else if available == 0 {
				e.ResolutionReason = "คาบเดิมมีชั่วโมงที่ส่งหรืออนุมัติแล้ว ต้องตีกลับก่อนจับคู่"
			} else {
				holidayDates := map[string]bool{}
				for _, o := range e.MatchOptions {
					holidayDates[o.OriginalDate] = true
				}
				switch {
				case len(holidayDates) > 1 || len(pendingDates[*e.MatchedSectionID]) > 1:
					e.ResolutionReason = "มีวันหยุดหรือวันชดเชยหลายวัน ต้องเลือกคาบเดิมเพื่อป้องกันการจับคู่ผิดวัน"
				case len(e.MatchOptions) != len(pendingSittings[*e.MatchedSectionID]):
					e.ResolutionReason = "จำนวนคาบวันหยุดกับรายการชดเชยไม่ตรงกัน กรุณาเลือกจับคู่รายคาบ"
				default:
					e.ResolutionReason = "ต้องเลือกคาบวันหยุดเดิม เนื่องจาก TDBM ไม่ระบุว่าชดเชยแทนคาบใด"
				}
			}
		}
	}
	return out, nil
}
