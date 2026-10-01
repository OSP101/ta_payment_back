package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/timeutil"
)

// HolidayService owns the public_holidays table + derived queries: "which
// holidays impact this course" and "throttled TA→lecturer reminders". Kept as
// a standalone service because both the worklog validator and the standalone
// admin/staff UI need the same read patterns.
type HolidayService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	notify *NotifyService
}

// Holiday is one row of public_holidays. Dates are strings ("YYYY-MM-DD") so
// callers can bind directly to JSON without a TZ round-trip that could shift
// the day for a Bangkok user.
type Holiday struct {
	ID          uuid.UUID `json:"id"`
	HolidayDate string    `json:"holiday_date"`
	NameTH      string    `json:"name_th"`
	NameEN      *string   `json:"name_en,omitempty"`
	Source      string    `json:"source"`
	Note        *string   `json:"note,omitempty"`
	// StartTime/EndTime ("HH:MM") narrow the holiday to part of the day — a
	// faculty ceremony that occupies the morning while the afternoon still
	// teaches. Both nil = closed all day, which is what every pre-0058 row is.
	// Only periods OVERLAPPING the window are blocked; see migration 0058.
	StartTime *string `json:"start_time,omitempty"`
	EndTime   *string `json:"end_time,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// List returns all holidays, optionally filtered to a single calendar year.
// Year==0 returns everything (used by "show all" and bulk-export flows).
func (s *HolidayService) List(ctx context.Context, year int) ([]Holiday, error) {
	q := `SELECT id, TO_CHAR(holiday_date,'YYYY-MM-DD'), name_th, name_en, source, note,
	             TO_CHAR(start_time,'HH24:MI'), TO_CHAR(end_time,'HH24:MI'),
	             TO_CHAR(created_at,'YYYY-MM-DD"T"HH24:MI:SS')
	      FROM public_holidays`
	args := []any{}
	if year > 0 {
		q += ` WHERE EXTRACT(YEAR FROM holiday_date) = $1`
		args = append(args, year)
	}
	q += ` ORDER BY holiday_date, name_th`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Holiday{}
	for rows.Next() {
		var h Holiday
		if err := rows.Scan(&h.ID, &h.HolidayDate, &h.NameTH, &h.NameEN, &h.Source, &h.Note,
			&h.StartTime, &h.EndTime, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

type HolidayInput struct {
	HolidayDate string  `json:"holiday_date" validate:"required,datetime=2006-01-02"`
	NameTH      string  `json:"name_th" validate:"required,max=200"`
	NameEN      *string `json:"name_en,omitempty" validate:"omitempty,max=200"`
	// Source defaults to "custom" server-side when empty; must otherwise match
	// the public_holidays_source_check constraint (migration 0035).
	Source string  `json:"source,omitempty" validate:"omitempty,oneof=national university faculty custom"`
	Note   *string `json:"note,omitempty" validate:"omitempty,max=1000"`
	// StartTime/EndTime ("HH:MM") — omit both for an all-day holiday. Only a
	// fast format check here; normalizeHolidayWindow still enforces "both or
	// neither" + end > start, which a per-field tag can't express.
	StartTime *string `json:"start_time,omitempty" validate:"omitempty,datetime=15:04"`
	EndTime   *string `json:"end_time,omitempty" validate:"omitempty,datetime=15:04"`
}

// validHolidaySource reports whether s is a type staff may enter by hand.
// Official closures come from TDBM only (migration 0126), so the CHECK
// constraint's 'national'/'university' stay valid for old rows but are no
// longer accepted here — typing one in would re-create the TDBM duplicates.
func validHolidaySource(s string) bool {
	switch s {
	case "faculty", "custom":
		return true
	}
	return false
}

// normalizeHolidayWindow validates the optional [start, end) time window and
// returns it normalized to "HH:MM" (or nil/nil for an all-day holiday). Empty
// strings are treated as absent so a form that always sends the field — but
// blank when the "all day" option is picked — doesn't have to null it out.
//
// Mirrors public_holidays_window_check (migration 0058): both or neither, and
// end strictly after start. Enforced here as well so the caller gets a Thai
// message instead of a raw constraint violation.
func normalizeHolidayWindow(start, end *string) (*string, *string, error) {
	trim := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	st, et := trim(start), trim(end)
	if st == "" && et == "" {
		return nil, nil, nil
	}
	if st == "" || et == "" {
		return nil, nil, Invalid("กรุณาระบุทั้งเวลาเริ่มและเวลาสิ้นสุดของช่วงวันหยุด (หรือเว้นว่างทั้งคู่ถ้าหยุดทั้งวัน)")
	}
	sm, ok1 := parseHM(st)
	em, ok2 := parseHM(et)
	if !ok1 || !ok2 {
		return nil, nil, Invalid("รูปแบบเวลาไม่ถูกต้อง (ต้องเป็น HH:MM)")
	}
	if sm >= em {
		return nil, nil, Invalid("เวลาสิ้นสุดของช่วงวันหยุดต้องมากกว่าเวลาเริ่ม")
	}
	// Re-emit from the parsed minutes so "9:00" and "09:00" store identically —
	// the unique index keys on the stored value.
	sOut := fmt.Sprintf("%02d:%02d", sm/60, sm%60)
	eOut := fmt.Sprintf("%02d:%02d", em/60, em%60)
	return &sOut, &eOut, nil
}

// holidayYearsAround bounds a holiday date to today ± this many years. Years
// 1900 and 9999 used to be accepted: nothing ever reads them, and a typo in
// the year (2096 for 2026) hides the real closure the faculty meant to add.
const holidayYearsAround = 5

// validateHolidayDate parses the date and refuses one outside the sane range.
func validateHolidayDate(date string) error {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return Invalid(fmt.Sprintf("วันที่ %q ไม่ถูกต้อง (ต้องเป็น YYYY-MM-DD)", date))
	}
	today := timeutil.Now()
	lo := time.Date(today.Year()-holidayYearsAround, 1, 1, 0, 0, 0, 0, time.UTC)
	hi := time.Date(today.Year()+holidayYearsAround, 12, 31, 0, 0, 0, 0, time.UTC)
	if d.Before(lo) || d.After(hi) {
		return Invalid(fmt.Sprintf("วันที่ %s อยู่นอกช่วงที่รับได้ (ปี ค.ศ. %d–%d) กรุณาตรวจปีอีกครั้ง",
			date, lo.Year(), hi.Year()))
	}
	return nil
}

// sameHolidayExists reports whether another closure already covers exactly
// this date and window, of ANY type. The unique index is per (date, source,
// window), so the same day entered once as "คณะ" and once as "อื่น ๆ" was two
// rows for one closure — every reader then counted it twice. exclude skips the
// row being edited (uuid.Nil on create).
func sameHolidayExists(ctx context.Context, q querier, date string, startT *string, exclude uuid.UUID) (string, bool, error) {
	var name string
	err := q.QueryRow(ctx, `
		SELECT name_th FROM public_holidays
		 WHERE holiday_date = $1::date
		   AND COALESCE(start_time, TIME '00:00') = COALESCE($2::time, TIME '00:00')
		   AND (start_time IS NULL) = ($2::time IS NULL)
		   AND id <> $3
		 LIMIT 1`, date, startT, exclude).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// holidayWindowLabelTH renders a window for user-facing messages: "ทั้งวัน" or
// "09:00–12:00". Used by every refusal that names a holiday, because "วันหยุด"
// alone is misleading once a holiday can cover only part of the day.
func holidayWindowLabelTH(start, end *string) string {
	if start == nil || end == nil {
		return "ทั้งวัน"
	}
	return *start + "–" + *end
}

// Create inserts a single holiday. Duplicate (date, source) → Invalid so the
// staff UI can surface a friendly message instead of a 500. `source` defaults
// to 'custom' when the caller omits it — safer than defaulting to 'national'
// which we treat as immutable in the admin UI.
func (s *HolidayService) Create(ctx context.Context, actor uuid.UUID, in HolidayInput) (uuid.UUID, error) {
	if err := validateHolidayDate(in.HolidayDate); err != nil {
		return uuid.Nil, err
	}
	if in.NameTH == "" {
		return uuid.Nil, Invalid("กรุณาระบุชื่อวันหยุด")
	}
	source := in.Source
	if source == "" {
		source = "custom"
	}
	if !validHolidaySource(source) {
		return uuid.Nil, Invalid("ประเภทวันหยุดไม่ถูกต้อง")
	}
	startT, endT, err := normalizeHolidayWindow(in.StartTime, in.EndTime)
	if err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "holiday.create", Entity: "holiday", EntityID: id.String()},
		func(tx pgx.Tx) error {
			if name, dup, err := sameHolidayExists(ctx, tx, in.HolidayDate, startT, uuid.Nil); err != nil {
				return err
			} else if dup {
				return Invalid(fmt.Sprintf("วันที่ %s (%s) มีวันหยุด \"%s\" อยู่แล้ว",
					thaiLongDateISO(in.HolidayDate), holidayWindowLabelTH(startT, endT), name))
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO public_holidays (id, holiday_date, name_th, name_en, source, note, start_time, end_time, created_by)
				 VALUES ($1, $2::date, $3, $4, $5, $6, $7::time, $8::time, $9)`,
				id, in.HolidayDate, in.NameTH, in.NameEN, source, in.Note, startT, endT, actor)
			if err != nil && !isUniqueViolation(err) {
				return err
			}
			if err != nil {
				// Postgres 23505 = unique_violation on (date, source, window). Two windows
				// on one date are allowed, so name the window in the message — otherwise
				// "มีวันหยุดอยู่แล้ว" reads as a lie to someone adding the afternoon half.
				return Invalid(fmt.Sprintf("มีวันหยุดสำหรับวันที่ %s (ประเภท%s, %s) อยู่แล้ว",
					thaiLongDateISO(in.HolidayDate), holidaySourceLabelTH(source), holidayWindowLabelTH(startT, endT)))
			}
			return clearHolidayClasses(ctx, tx, in.HolidayDate, startT, endT)
		}); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// clearHolidayClasses runs in the same transaction as a new closure. Work
// logged for a class sitting that the closure now cancels would otherwise
// stay payable: Submit does not re-validate drafts, and sent/approved rows
// are never re-checked at all — so a holiday added after the fact paid the
// cancelled class AND, once the holiday page asked for one, its makeup.
//
// Sent or approved sittings refuse the closure (a lecturer has to bounce them
// first, so the decision is visible); unsent drafts of lecture/lab sittings
// inside the closure are removed.
func clearHolidayClasses(ctx context.Context, tx pgx.Tx, date string, startT, endT *string) error {
	overlap := `wl.work_date = $1::date AND wl.activity IN ('lecture','lab')
	            AND ($2::time IS NULL OR $3::time IS NULL
	                 OR (wl.start_time < $3::time AND wl.end_time > $2::time))`
	var sent int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM work_logs wl WHERE `+overlap+
		` AND wl.status IN ('submitted','approved')`, date, startT, endT).Scan(&sent); err != nil {
		return err
	}
	if sent > 0 {
		return Conflict(fmt.Sprintf(
			"วันที่ %s มีบันทึกเวลาคาบบรรยาย/ปฏิบัติการที่ส่งหรืออนุมัติแล้ว %d รายการ ถ้าวันนั้นเป็นวันหยุดจริง ให้อาจารย์ตีกลับรายการเหล่านั้นก่อน แล้วจึงเพิ่มวันหยุด",
			date, sent))
	}
	_, err := tx.Exec(ctx, `DELETE FROM work_logs wl WHERE `+overlap+` AND wl.status = 'draft'`, date, startT, endT)
	return err
}

// BulkCreate inserts many holidays at once, silently skipping duplicates via
// ON CONFLICT so importing the same list twice is a no-op. Returns the count
// actually inserted.
func (s *HolidayService) BulkCreate(ctx context.Context, actor uuid.UUID, ins []HolidayInput) (int, error) {
	if len(ins) == 0 {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	inserted := 0
	for _, in := range ins {
		if err := validateHolidayDate(in.HolidayDate); err != nil {
			return 0, err
		}
		if in.NameTH == "" {
			return 0, Invalid("กรุณาระบุชื่อวันหยุดทุกแถว")
		}
		source := in.Source
		if source == "" {
			source = "custom"
		}
		if !validHolidaySource(source) {
			return 0, Invalid("ประเภทวันหยุดไม่ถูกต้อง")
		}
		startT, endT, err := normalizeHolidayWindow(in.StartTime, in.EndTime)
		if err != nil {
			return 0, err
		}
		// The same closure under another type is a duplicate too — skipped,
		// like the same-type duplicate ON CONFLICT already skips, so importing
		// a list twice stays a no-op.
		if _, dup, err := sameHolidayExists(ctx, tx, in.HolidayDate, startT, uuid.Nil); err != nil {
			return 0, err
		} else if dup {
			continue
		}
		// Conflict target must name the index EXPRESSION, not the bare columns —
		// the arbiter is the partial-window unique index from migration 0058.
		tag, err := tx.Exec(ctx,
			`INSERT INTO public_holidays (holiday_date, name_th, name_en, source, note, start_time, end_time, created_by)
			 VALUES ($1::date, $2, $3, $4, $5, $6::time, $7::time, $8)
			 ON CONFLICT (holiday_date, source, (COALESCE(start_time, TIME '00:00'))) DO NOTHING`,
			in.HolidayDate, in.NameTH, in.NameEN, source, in.Note, startT, endT, actor)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() > 0 {
			if err := clearHolidayClasses(ctx, tx, in.HolidayDate, startT, endT); err != nil {
				return 0, err
			}
		}
		inserted += int(tag.RowsAffected())
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "holiday.bulk_create", Entity: "holiday", Note: fmt.Sprintf("inserted %d/%d", inserted, len(ins))}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return inserted, nil
}

// Patch updates a holiday's name/note and its time window. Changing the DATE is
// still disallowed — makeups + worklogs reference the date directly and there is
// no cheap way to cascade the rename; delete + create is the sanctioned flow.
//
// The window is editable, unlike the date, because getting it wrong is the
// expected mistake ("the ceremony runs till 13:00, not 12:00") and the blast
// radius is bounded: nothing stores a copy of it. Validation re-runs on every
// worklog write, so a corrected window takes effect immediately — a class that
// was blocked becomes loggable, and vice versa.
func (s *HolidayService) Patch(ctx context.Context, actor, id uuid.UUID, nameTH string, nameEN, note, startTime, endTime *string) error {
	if nameTH == "" {
		return Invalid("กรุณาระบุชื่อวันหยุด")
	}
	startT, endT, err := normalizeHolidayWindow(startTime, endTime)
	if err != nil {
		return err
	}
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "holiday.patch", Entity: "holiday", EntityID: id.String()},
		func(tx pgx.Tx) error {
			var date string
			err := tx.QueryRow(ctx,
				`UPDATE public_holidays SET name_th=$1, name_en=$2, note=$3, start_time=$4::time, end_time=$5::time
				 WHERE id=$6 RETURNING holiday_date::text`,
				nameTH, nameEN, note, startT, endT, id).Scan(&date)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err == nil {
				if name, dup, derr := sameHolidayExists(ctx, tx, date, startT, id); derr != nil {
					return derr
				} else if dup {
					return Invalid(fmt.Sprintf("วันนี้มีวันหยุด \"%s\" ในช่วงเวลา %s อยู่แล้ว", name, holidayWindowLabelTH(startT, endT)))
				}
			}
			if isUniqueViolation(err) {
				// The edited window now collides with another row on the same date+source.
				return Invalid(fmt.Sprintf("มีวันหยุดของวันนี้ในช่วงเวลา %s อยู่แล้ว", holidayWindowLabelTH(startT, endT)))
			}
			if err != nil {
				return err
			}
			// Widening the window (or making it whole-day) cancels more sittings,
			// exactly like a new closure — same refusal and draft cleanup as Create.
			return clearHolidayClasses(ctx, tx, date, startT, endT)
		})
}

func (s *HolidayService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	return writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &actor, Action: "holiday.delete", Entity: "holiday", EntityID: id.String()},
		func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM public_holidays WHERE id=$1`, id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			return nil
		})
}

// (An InRange helper used to live here, returning date→name and nothing else.
// Nothing called it — the worklog validator has its own loadHolidaysInRange —
// and a date-keyed map cannot express a partial-day holiday at all, so it was
// removed rather than left as a window-blind shortcut for the next caller to
// reach for. Use WorkLogService.loadHolidaysInRange / holidaySet.)

// ---------------------------------------------------------------------------
// Holiday impact — the per-course join used by the TA/lecturer holidays page
// ---------------------------------------------------------------------------

type HolidayImpactMakeup struct {
	ID uuid.UUID `json:"id"`
	// MakeupDate is nil when Waived is true — a waiver has no replacement date.
	MakeupDate *string `json:"makeup_date,omitempty"`
	StartTime  *string `json:"start_time,omitempty"`
	EndTime    *string `json:"end_time,omitempty"`
	Note       *string `json:"note,omitempty"`
	// Waived means a manager/TA confirmed this period will deliberately not
	// get a makeup (migration 0104) — treated as resolved, same as a filed one.
	Waived bool `json:"waived"`
}

type HolidayImpactSection struct {
	SectionID uuid.UUID            `json:"section_id"`
	SecNo     string               `json:"sec_no"`
	Track     string               `json:"track"`
	Kind      string               `json:"kind"`
	StartTime string               `json:"start_time"`
	EndTime   string               `json:"end_time"`
	Room      *string              `json:"room,omitempty"`
	Makeup    *HolidayImpactMakeup `json:"makeup"`
}

type HolidayImpact struct {
	OriginalDate  string `json:"original_date"`
	DayOfWeek     int    `json:"day_of_week"`
	HolidayNameTH string `json:"holiday_name_th"`
	// HolidayStart/HolidayEnd are the closure's time window ("HH:MM"), nil for a
	// whole-day holiday. Both UIs render it next to the name — with partial-day
	// holidays in play, a date alone no longer tells the lecturer which of the
	// day's periods actually died.
	HolidayStart     *string                `json:"holiday_start,omitempty"`
	HolidayEnd       *string                `json:"holiday_end,omitempty"`
	AffectedSections []HolidayImpactSection `json:"affected_sections"`
}

type HolidayImpactsResponse struct {
	Impacts         []HolidayImpact `json:"impacts"`
	UnresolvedCount int             `json:"unresolved_count"`
	// OtherMakeups are the course's makeups and waivers that are NOT attached
	// to a holiday above — a class cancelled for another reason (the lecturer
	// was away), or one whose holiday was since deleted. They used to be
	// invisible: a waiver of an ordinary teaching day could be filed through
	// the API but never seen or undone on the page.
	OtherMakeups []CourseMakeupRow `json:"other_makeups"`
}

// CourseMakeupRow is one makeup_schedules row as the lecturer's list shows it.
type CourseMakeupRow struct {
	ID           uuid.UUID `json:"id"`
	SectionID    uuid.UUID `json:"section_id"`
	SecNo        string    `json:"sec_no"`
	Kind         string    `json:"kind"`
	OriginalDate string    `json:"original_date"`
	MakeupDate   *string   `json:"makeup_date,omitempty"`
	StartTime    *string   `json:"start_time,omitempty"`
	EndTime      *string   `json:"end_time,omitempty"`
	Note         *string   `json:"note,omitempty"`
	Waived       bool      `json:"waived"`
}

// ImpactsForCourse computes every holiday in the course's term that lands on a
// scheduled class day + which sections/kinds it affects + whether a makeup was
// filed. Read by both the TA (view-only) and lecturer (edit) holidays pages.
func (s *HolidayService) ImpactsForCourse(ctx context.Context, tcID uuid.UUID) (*HolidayImpactsResponse, error) {
	// Resolve the course window first. The fallback is the parent ACADEMIC TERM,
	// not CURRENT_DATE: every course imported from the registrar leaves its own
	// starts_on/ends_on NULL, and falling back to today gave a one-day window that
	// matched nothing — so this page reported "no holidays" on courses whose
	// sidebar badge (which used the term fallback) said 8. See CourseStartSQL.
	var termStart, termEnd time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT `+CourseStartSQL("tc")+`, `+CourseEndSQL("tc")+`
		 FROM teaching_courses tc WHERE tc.id = $1`, tcID).Scan(&termStart, &termEnd); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// Join public_holidays × section_schedules on day_of_week, keeping only
	// dates in the term window. LEFT JOIN makeup_schedules pulls in any filed
	// makeup for the (section, holiday) pair.
	rows, err := s.pool.Query(ctx, `
		SELECT TO_CHAR(h.holiday_date,'YYYY-MM-DD') AS original_date,
		       EXTRACT(DOW FROM h.holiday_date)::int AS dow,
		       h.name_th,
		       TO_CHAR(h.start_time,'HH24:MI'), TO_CHAR(h.end_time,'HH24:MI'),
		       sec.id, sec.sec_no, sec.track::text,
		       sch.kind, sch.start_time::text, sch.end_time::text, sch.room,
		       m.id,
		       CASE WHEN m.makeup_date IS NULL THEN NULL ELSE TO_CHAR(m.makeup_date,'YYYY-MM-DD') END,
		       m.start_time::text, m.end_time::text, m.note, m.waived
		FROM public_holidays h
		JOIN sections sec ON sec.teaching_course_id = $1
		JOIN section_schedules sch
		     ON sch.section_id = sec.id
		     AND sch.day_of_week = EXTRACT(DOW FROM h.holiday_date)::int
		     -- Partial-day holiday: only periods that OVERLAP the closure are
		     -- affected. A faculty ceremony 08:00–12:00 does not touch the 13:00
		     -- lab, so listing it here would send the lecturer off to reschedule
		     -- a class that is still running. NULL window = all day = matches
		     -- every period (migration 0058).
		     AND (h.start_time IS NULL
		          OR (sch.start_time < h.end_time AND sch.end_time > h.start_time))
		-- kind is part of the match: each period of the day has its own makeup,
		-- with its own replacement slot. Without it the lab's makeup appeared on
		-- the lecture row, claiming both would run in the same slot.
		LEFT JOIN makeup_schedules m
		     ON m.section_id = sec.id
		     AND m.original_date = h.holiday_date
		     AND m.kind = sch.kind
		WHERE h.holiday_date BETWEEN $2::date AND $3::date
		ORDER BY h.holiday_date, h.start_time NULLS FIRST, sec.sec_no, sch.kind`,
		tcID, termStart.Format("2006-01-02"), termEnd.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Bucket rows into impact groups. Keyed by (date, window) rather than date
	// alone: one date can carry two faculty closures — a morning ceremony and a
	// late-afternoon event — which affect different periods and are separate
	// things to reschedule. Keeping insertion order via a slice of keys because
	// Go maps don't preserve iteration order.
	group := map[string]*HolidayImpact{}
	order := []string{}
	unresolved := 0
	for rows.Next() {
		var origDate string
		var dow int
		var nameTH string
		var hStart, hEnd *string
		var sec HolidayImpactSection
		var trackStr string
		var mID *uuid.UUID
		var mDate, mStart, mEnd *string
		var mNote *string
		var mWaived *bool
		if err := rows.Scan(&origDate, &dow, &nameTH, &hStart, &hEnd,
			&sec.SectionID, &sec.SecNo, &trackStr,
			&sec.Kind, &sec.StartTime, &sec.EndTime, &sec.Room,
			&mID, &mDate, &mStart, &mEnd, &mNote, &mWaived); err != nil {
			return nil, err
		}
		sec.Track = trackStr
		// mID alone (not mID + mDate) marks resolved: a waived row has an id but
		// no makeup_date, and must count the same as a filed one — that's the
		// whole point of the waiver (see migration 0104). mWaived comes back NULL
		// (LEFT JOIN, no matching row) exactly when mID does, so it's never nil
		// on the branch that reads it.
		if mID != nil {
			sec.Makeup = &HolidayImpactMakeup{
				ID:         *mID,
				MakeupDate: mDate,
				StartTime:  mStart,
				EndTime:    mEnd,
				Note:       mNote,
				Waived:     mWaived != nil && *mWaived,
			}
		} else {
			unresolved++
		}
		gkey := origDate + "|" + holidayWindowLabelTH(hStart, hEnd)
		g, ok := group[gkey]
		if !ok {
			g = &HolidayImpact{
				OriginalDate:  origDate,
				DayOfWeek:     dow,
				HolidayNameTH: nameTH,
				HolidayStart:  hStart,
				HolidayEnd:    hEnd,
			}
			group[gkey] = g
			order = append(order, gkey)
		}
		g.AffectedSections = append(g.AffectedSections, sec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	impacts := make([]HolidayImpact, 0, len(order))
	shown := map[uuid.UUID]struct{}{}
	for _, k := range order {
		impacts = append(impacts, *group[k])
		for _, sec := range group[k].AffectedSections {
			if sec.Makeup != nil {
				shown[sec.Makeup.ID] = struct{}{}
			}
		}
	}
	other, err := s.courseMakeupsExcept(ctx, tcID, shown)
	if err != nil {
		return nil, err
	}
	return &HolidayImpactsResponse{Impacts: impacts, UnresolvedCount: unresolved, OtherMakeups: other}, nil
}

// courseMakeupsExcept lists every makeup/waiver of the course not already in
// shown, oldest cancelled day first.
func (s *HolidayService) courseMakeupsExcept(ctx context.Context, tcID uuid.UUID, shown map[uuid.UUID]struct{}) ([]CourseMakeupRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, sec.id, sec.sec_no, m.kind,
		       TO_CHAR(m.original_date,'YYYY-MM-DD'),
		       CASE WHEN m.makeup_date IS NULL THEN NULL ELSE TO_CHAR(m.makeup_date,'YYYY-MM-DD') END,
		       TO_CHAR(m.start_time,'HH24:MI'), TO_CHAR(m.end_time,'HH24:MI'),
		       m.note, m.waived
		  FROM makeup_schedules m
		  JOIN sections sec ON sec.id = m.section_id
		 WHERE sec.teaching_course_id = $1
		 ORDER BY m.original_date, sec.sec_no, m.kind`, tcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CourseMakeupRow{}
	for rows.Next() {
		var r CourseMakeupRow
		if err := rows.Scan(&r.ID, &r.SectionID, &r.SecNo, &r.Kind, &r.OriginalDate,
			&r.MakeupDate, &r.StartTime, &r.EndTime, &r.Note, &r.Waived); err != nil {
			return nil, err
		}
		if _, ok := shown[r.ID]; ok {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Remind — TA nudges lecturer(s) to file a makeup
// ---------------------------------------------------------------------------

// RemindLecturer sends an in-app notification to every lecturer of the course,
// throttled to at most one send per (TA, course, original_date) per 24 hours.
// Returns Invalid("ส่งไปแล้ววันนี้ …") when the throttle rejects the request so
// the frontend can surface a nice message rather than a raw 429.
func (s *HolidayService) RemindLecturer(ctx context.Context, taID, tcID uuid.UUID, originalDate string, note string) error {
	if _, err := time.Parse("2006-01-02", originalDate); err != nil {
		return Invalid("รูปแบบวันที่ไม่ถูกต้อง")
	}
	// Ensure the caller is genuinely a TA on this course. Cross-check against
	// approved assignments so a random TA on another course can't spam.
	var owns bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM ta_request_assignments a
			JOIN sections sec ON sec.id = a.section_id
			JOIN ta_requests r ON r.id = a.request_id
			WHERE a.ta_id = $1 AND sec.teaching_course_id = $2 AND r.status = 'approved'
		)`, taID, tcID).Scan(&owns); err != nil {
		return err
	}
	if !owns {
		return ErrForbidden
	}
	// Only a real closure can need a makeup. Without this the throttle below —
	// keyed by date — was trivially sidestepped by picking another date, and a
	// TA could mail the lecturer once per calendar day of the term.
	var isHoliday bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM public_holidays WHERE holiday_date = $1::date)`,
		originalDate).Scan(&isHoliday); err != nil {
		return err
	}
	if !isHoliday {
		return Invalid("วันที่เลือกไม่ใช่วันหยุด จึงไม่ต้องกำหนดวันชดเชย")
	}
	// Throttle: reject if we sent a reminder for this trio in the last 24h.
	var lastSent *time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT MAX(sent_at) FROM holiday_remind_log
		WHERE ta_id = $1 AND teaching_course_id = $2 AND original_date = $3::date`,
		taID, tcID, originalDate).Scan(&lastSent); err != nil {
		return err
	}
	if lastSent != nil && time.Since(*lastSent) < 24*time.Hour {
		return Invalid("ส่งแจ้งเตือนไปแล้ววันนี้ จะส่งได้อีกครั้งใน 24 ชั่วโมง")
	}

	// Look up the TA's name + course code for the notification body.
	// The name used to come from users.prefix, a column that does not exist:
	// the error was discarded and every notice named nobody.
	var courseCode, courseNameTH, holidayName string
	taName := personName(ctx, s.pool, taID)
	_ = s.pool.QueryRow(ctx, `
		SELECT tc.code, tc.name_th
		FROM teaching_courses tc
		WHERE tc.id = $1`, tcID).Scan(&courseCode, &courseNameTH)
	// A date can now carry several closures (all-day + a faculty window, or two
	// faculty windows). Name them all with their hours — "วันกีฬาคณะ" alone leaves
	// the lecturer guessing which period the TA means.
	_ = s.pool.QueryRow(ctx, `
		SELECT string_agg(
		         name_th || CASE WHEN start_time IS NULL THEN ''
		                         ELSE ' ' || TO_CHAR(start_time,'HH24:MI') || '–' || TO_CHAR(end_time,'HH24:MI')
		                    END,
		         ', ' ORDER BY start_time NULLS FIRST)
		  FROM public_holidays WHERE holiday_date = $1::date`,
		originalDate).Scan(&holidayName)

	// Fan-out to every lecturer teaching the course.
	rows, err := s.pool.Query(ctx,
		`SELECT lecturer_id FROM teaching_lecturers WHERE teaching_course_id = $1`, tcID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var lecturerIDs []uuid.UUID
	for rows.Next() {
		var lid uuid.UUID
		if err := rows.Scan(&lid); err != nil {
			return err
		}
		lecturerIDs = append(lecturerIDs, lid)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(lecturerIDs) == 0 {
		return Invalid("ไม่พบอาจารย์ประจำวิชา")
	}
	body := fmt.Sprintf(
		"%s ผู้ช่วยสอนรายวิชา %s %s แจ้งว่ายังไม่มีการกำหนดวันสอนชดเชยสำหรับวันที่ %s (%s)",
		taName, courseCode, courseNameTH, thaiLongDateISO(originalDate), holidayName,
	)
	if note != "" {
		body += "\nหมายเหตุ: " + note
	}
	link := fmt.Sprintf("/lecturer/courses/%s/holidays", tcID.String())
	// Audit + rate-limit ledger FIRST. The ledger is what stops a TA sending the
	// same reminder repeatedly; mailing before it was written meant a failed
	// insert returned an error, the TA retried, and the lecturers were mailed
	// again with nothing recorded.
	if err := writeAudited(ctx, s.pool, s.aud,
		audit.Entry{ActorID: &taID, Action: "holiday.remind", Entity: "teaching_course", EntityID: tcID.String(), Note: originalDate},
		func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO holiday_remind_log (ta_id, teaching_course_id, original_date, note)
				 VALUES ($1, $2, $3::date, $4)`,
				taID, tcID, originalDate, nilStrOrEmpty(note))
			return err
		}); err != nil {
		return err
	}
	if s.notify != nil {
		for _, lid := range lecturerIDs {
			s.notify.SendAction(ctx, lid, "ผู้ช่วยสอนแจ้งให้กำหนดวันสอนชดเชย "+thaiLongDateISO(originalDate), body, link)
		}
	}
	return nil
}

func nilStrOrEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// holidaySourceLabelTH is the staff holiday page's own wording for a source
// (SOURCE_OPTIONS in app/staff/holidays/page.tsx); the raw enum ("custom")
// leaked into a Thai error message.
func holidaySourceLabelTH(source string) string {
	switch source {
	case "national":
		return "ราชการ"
	case "university":
		return "มหาวิทยาลัย"
	case "faculty":
		return "คณะ"
	case "tdbm":
		return "TDBM"
	default:
		return "อื่นๆ"
	}
}
