package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ta-payment-back/internal/audit"
)

// HolidayService owns the public_holidays table + derived queries: "which
// holidays impact this course" and "throttled TA→lecturer reminders". Kept as
// a standalone service because both the worklog validator and the standalone
// admin/staff UI need the same read patterns.
type HolidayService struct {
	pool   *pgxpool.Pool
	aud    *audit.Auditor
	notify *NotifyService
	// BOT (Bank of Thailand) Open API — used by SyncFromBOT.
	botAPIBaseURL  string
	botAPIClientID string
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
	CreatedAt   string    `json:"created_at"`
}

// List returns all holidays, optionally filtered to a single calendar year.
// Year==0 returns everything (used by "show all" and bulk-export flows).
func (s *HolidayService) List(ctx context.Context, year int) ([]Holiday, error) {
	q := `SELECT id, TO_CHAR(holiday_date,'YYYY-MM-DD'), name_th, name_en, source, note, TO_CHAR(created_at,'YYYY-MM-DD"T"HH24:MI:SS')
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
		if err := rows.Scan(&h.ID, &h.HolidayDate, &h.NameTH, &h.NameEN, &h.Source, &h.Note, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

type HolidayInput struct {
	HolidayDate string  `json:"holiday_date"`
	NameTH      string  `json:"name_th"`
	NameEN      *string `json:"name_en,omitempty"`
	Source      string  `json:"source,omitempty"`
	Note        *string `json:"note,omitempty"`
}

// validHolidaySource reports whether s is one of the allowed holiday types.
// Must stay in sync with the CHECK constraint on public_holidays.source.
func validHolidaySource(s string) bool {
	switch s {
	case "national", "university", "faculty", "custom":
		return true
	}
	return false
}

// Create inserts a single holiday. Duplicate (date, source) → Invalid so the
// staff UI can surface a friendly message instead of a 500. `source` defaults
// to 'custom' when the caller omits it — safer than defaulting to 'national'
// which we treat as immutable in the admin UI.
func (s *HolidayService) Create(ctx context.Context, actor uuid.UUID, in HolidayInput) (uuid.UUID, error) {
	if _, err := time.Parse("2006-01-02", in.HolidayDate); err != nil {
		return uuid.Nil, Invalid("รูปแบบวันที่ไม่ถูกต้อง (ต้องเป็น YYYY-MM-DD)")
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
	id := uuid.New()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO public_holidays (id, holiday_date, name_th, name_en, source, note, created_by)
		 VALUES ($1, $2::date, $3, $4, $5, $6, $7)`,
		id, in.HolidayDate, in.NameTH, in.NameEN, source, in.Note, actor)
	if err != nil {
		// Postgres error code 23505 = unique_violation on (holiday_date, source).
		return uuid.Nil, Invalid(fmt.Sprintf("มีวันหยุดสำหรับวันที่ %s (%s) อยู่แล้ว", in.HolidayDate, source))
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "holiday.create", Entity: "holiday", EntityID: id.String()})
	return id, nil
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
		if _, err := time.Parse("2006-01-02", in.HolidayDate); err != nil {
			return 0, Invalid(fmt.Sprintf("วันที่ %q ไม่ถูกต้อง", in.HolidayDate))
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
		tag, err := tx.Exec(ctx,
			`INSERT INTO public_holidays (holiday_date, name_th, name_en, source, note, created_by)
			 VALUES ($1::date, $2, $3, $4, $5, $6)
			 ON CONFLICT (holiday_date, source) DO NOTHING`,
			in.HolidayDate, in.NameTH, in.NameEN, source, in.Note, actor)
		if err != nil {
			return 0, err
		}
		inserted += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "holiday.bulk_create", Entity: "holiday", Note: fmt.Sprintf("inserted %d/%d", inserted, len(ins))})
	return inserted, nil
}

// Patch updates a holiday's name/note only. Changing the date is disallowed —
// makeups + worklogs reference the date directly and there is no cheap way to
// cascade the rename. Delete + create is the sanctioned flow.
func (s *HolidayService) Patch(ctx context.Context, actor, id uuid.UUID, nameTH string, nameEN, note *string) error {
	if nameTH == "" {
		return Invalid("กรุณาระบุชื่อวันหยุด")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE public_holidays SET name_th=$1, name_en=$2, note=$3 WHERE id=$4`,
		nameTH, nameEN, note, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "holiday.patch", Entity: "holiday", EntityID: id.String()})
	return nil
}

func (s *HolidayService) Delete(ctx context.Context, actor, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM public_holidays WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &actor, Action: "holiday.delete", Entity: "holiday", EntityID: id.String()})
	return nil
}

// InRange returns a map of "YYYY-MM-DD" -> Thai name for every holiday whose
// date falls in [start, end] inclusive. Used by the worklog validator to
// short-circuit an entry on a holiday date; the caller passes the term start/
// end so the query stays tight (< 30 rows for a semester).
func (s *HolidayService) InRange(ctx context.Context, start, end time.Time) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT TO_CHAR(holiday_date,'YYYY-MM-DD'), name_th
		 FROM public_holidays
		 WHERE holiday_date BETWEEN $1::date AND $2::date`,
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var date, name string
		if err := rows.Scan(&date, &name); err != nil {
			return nil, err
		}
		// If two rows fall on the same date (e.g. national + university), keep
		// the first — the surfaced name is informational, and both apply as
		// "cannot log" anyway.
		if _, exists := out[date]; !exists {
			out[date] = name
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Holiday impact — the per-course join used by the TA/lecturer holidays page
// ---------------------------------------------------------------------------

type HolidayImpactMakeup struct {
	ID         uuid.UUID `json:"id"`
	MakeupDate string    `json:"makeup_date"`
	StartTime  *string   `json:"start_time,omitempty"`
	EndTime    *string   `json:"end_time,omitempty"`
	Note       *string   `json:"note,omitempty"`
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
	OriginalDate     string                 `json:"original_date"`
	DayOfWeek        int                    `json:"day_of_week"`
	HolidayNameTH    string                 `json:"holiday_name_th"`
	AffectedSections []HolidayImpactSection `json:"affected_sections"`
}

type HolidayImpactsResponse struct {
	Impacts         []HolidayImpact `json:"impacts"`
	UnresolvedCount int             `json:"unresolved_count"`
}

// ImpactsForCourse computes every holiday in the course's term that lands on a
// scheduled class day + which sections/kinds it affects + whether a makeup was
// filed. Read by both the TA (view-only) and lecturer (edit) holidays pages.
func (s *HolidayService) ImpactsForCourse(ctx context.Context, tcID uuid.UUID) (*HolidayImpactsResponse, error) {
	// Resolve term dates first — a course without dates has no window to check.
	var termStart, termEnd time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(starts_on, CURRENT_DATE), COALESCE(ends_on, CURRENT_DATE)
		 FROM teaching_courses WHERE id = $1`, tcID).Scan(&termStart, &termEnd); err != nil {
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
		       sec.id, sec.sec_no, sec.track::text,
		       sch.kind, sch.start_time::text, sch.end_time::text, sch.room,
		       m.id,
		       CASE WHEN m.id IS NULL THEN NULL ELSE TO_CHAR(m.makeup_date,'YYYY-MM-DD') END,
		       m.start_time::text, m.end_time::text, m.note
		FROM public_holidays h
		JOIN sections sec ON sec.teaching_course_id = $1
		JOIN section_schedules sch
		     ON sch.section_id = sec.id
		     AND sch.day_of_week = EXTRACT(DOW FROM h.holiday_date)::int
		LEFT JOIN makeup_schedules m
		     ON m.section_id = sec.id
		     AND m.original_date = h.holiday_date
		WHERE h.holiday_date BETWEEN $2::date AND $3::date
		ORDER BY h.holiday_date, sec.sec_no, sch.kind`,
		tcID, termStart.Format("2006-01-02"), termEnd.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Bucket rows into per-date impact groups. Keep insertion order via a slice
	// of keys — Go maps don't preserve iteration order.
	group := map[string]*HolidayImpact{}
	order := []string{}
	unresolved := 0
	for rows.Next() {
		var origDate string
		var dow int
		var nameTH string
		var sec HolidayImpactSection
		var trackStr string
		var mID *uuid.UUID
		var mDate, mStart, mEnd *string
		var mNote *string
		if err := rows.Scan(&origDate, &dow, &nameTH,
			&sec.SectionID, &sec.SecNo, &trackStr,
			&sec.Kind, &sec.StartTime, &sec.EndTime, &sec.Room,
			&mID, &mDate, &mStart, &mEnd, &mNote); err != nil {
			return nil, err
		}
		sec.Track = trackStr
		if mID != nil && mDate != nil {
			sec.Makeup = &HolidayImpactMakeup{
				ID:         *mID,
				MakeupDate: *mDate,
				StartTime:  mStart,
				EndTime:    mEnd,
				Note:       mNote,
			}
		} else {
			unresolved++
		}
		g, ok := group[origDate]
		if !ok {
			g = &HolidayImpact{OriginalDate: origDate, DayOfWeek: dow, HolidayNameTH: nameTH}
			group[origDate] = g
			order = append(order, origDate)
		}
		g.AffectedSections = append(g.AffectedSections, sec)
	}
	impacts := make([]HolidayImpact, 0, len(order))
	for _, k := range order {
		impacts = append(impacts, *group[k])
	}
	return &HolidayImpactsResponse{Impacts: impacts, UnresolvedCount: unresolved}, nil
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
	// Throttle: reject if we sent a reminder for this trio in the last 24h.
	var lastSent *time.Time
	if err := s.pool.QueryRow(ctx, `
		SELECT MAX(sent_at) FROM holiday_remind_log
		WHERE ta_id = $1 AND teaching_course_id = $2 AND original_date = $3::date`,
		taID, tcID, originalDate).Scan(&lastSent); err != nil {
		return err
	}
	if lastSent != nil && time.Since(*lastSent) < 24*time.Hour {
		return Invalid("ส่งแจ้งเตือนไปแล้ววันนี้ — จะส่งได้อีกครั้งใน 24 ชั่วโมง")
	}

	// Look up the TA's name + course code for the notification body.
	var taName, courseCode, courseNameTH, holidayName string
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(prefix,'') || ' ' || first_name || ' ' || last_name
		FROM users WHERE id = $1`, taID).Scan(&taName)
	_ = s.pool.QueryRow(ctx, `
		SELECT tc.code, tc.name_th
		FROM teaching_courses tc
		WHERE tc.id = $1`, tcID).Scan(&courseCode, &courseNameTH)
	_ = s.pool.QueryRow(ctx,
		`SELECT name_th FROM public_holidays WHERE holiday_date = $1::date LIMIT 1`,
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
	if len(lecturerIDs) == 0 {
		return Invalid("ไม่พบอาจารย์ประจำวิชา")
	}
	body := fmt.Sprintf(
		"%s แจ้งว่ายังไม่ได้กำหนดวันชดเชยของวันที่ %s (%s) วิชา %s — %s",
		taName, originalDate, holidayName, courseCode, courseNameTH,
	)
	if note != "" {
		body += " · หมายเหตุ: " + note
	}
	link := fmt.Sprintf("/lecturer/courses/%s/holidays", tcID.String())
	if s.notify != nil {
		for _, lid := range lecturerIDs {
			s.notify.Send(ctx, lid, "TA แจ้งให้กำหนดวันชดเชย", body, link)
		}
	}
	// Audit + rate-limit ledger
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO holiday_remind_log (ta_id, teaching_course_id, original_date, note)
		 VALUES ($1, $2, $3::date, $4)`,
		taID, tcID, originalDate, nilStrOrEmpty(note)); err != nil {
		return err
	}
	s.aud.Log(ctx, audit.Entry{ActorID: &taID, Action: "holiday.remind", Entity: "teaching_course", EntityID: tcID.String(), Note: originalDate})
	return nil
}

func nilStrOrEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// SyncFromBOT — pull national holidays from Bank of Thailand's Open API
// ---------------------------------------------------------------------------

type SyncFromBOTResult struct {
	Fetched  int `json:"fetched"`
	Inserted int `json:"inserted"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
}

// botHolidayRow mirrors one entry from https://apiportal.bot.or.th/ →
// "Financial Institutions' Holidays". Only the fields we consume are declared.
type botHolidayRow struct {
	Date                   string `json:"Date"`
	HolidayDescription     string `json:"HolidayDescription"`
	HolidayDescriptionThai string `json:"HolidayDescriptionThai"`
}

// SyncFromBOT fetches BOT's holiday list for a calendar year and merges it into
// public_holidays (source='national') using a smart upsert:
//   - Missing (date, source='national') → INSERT with name_th + name_en from BOT
//   - Existing row with name_en IS NULL → UPDATE only name_en (assume staff may
//     have already edited name_th / note; never overwrite those)
//   - Existing row with name_en set → SKIP entirely
func (s *HolidayService) SyncFromBOT(ctx context.Context, actor uuid.UUID, year int) (SyncFromBOTResult, error) {
	var zero SyncFromBOTResult
	if year < 1900 || year > 3000 {
		return zero, Invalid("ปีไม่ถูกต้อง")
	}
	if strings.TrimSpace(s.botAPIClientID) == "" {
		return zero, &UserError{Status: 503, Msg: "ยังไม่ได้ตั้งค่า BOT API key (BOT_API_CLIENT_ID) ในไฟล์ .env ของเซิร์ฟเวอร์"}
	}

	rows, err := s.fetchBOTHolidays(ctx, year)
	if err != nil {
		return zero, err
	}
	result := SyncFromBOTResult{Fetched: len(rows)}
	if len(rows) == 0 {
		return result, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)

	for _, r := range rows {
		date := strings.TrimSpace(r.Date)
		nameTH := strings.TrimSpace(r.HolidayDescriptionThai)
		nameEN := strings.TrimSpace(r.HolidayDescription)
		if date == "" || nameTH == "" {
			// Defensive: BOT sometimes emits empty entries; skip silently.
			result.Skipped++
			continue
		}
		if _, err := time.Parse("2006-01-02", date); err != nil {
			result.Skipped++
			continue
		}

		// Look up existing (date, source='national') row.
		var existingID uuid.UUID
		var existingNameEN *string
		err := tx.QueryRow(ctx,
			`SELECT id, name_en FROM public_holidays
			 WHERE holiday_date = $1::date AND source = 'national'`, date).
			Scan(&existingID, &existingNameEN)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Fresh insert — full row, actor as creator.
			var nameENArg any
			if nameEN != "" {
				nameENArg = nameEN
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO public_holidays (holiday_date, name_th, name_en, source, created_by)
				 VALUES ($1::date, $2, $3, 'national', $4)
				 ON CONFLICT (holiday_date, source) DO NOTHING`,
				date, nameTH, nameENArg, actor); err != nil {
				return zero, err
			}
			result.Inserted++
		case err != nil:
			return zero, err
		default:
			// Row exists — fill name_en only if it was NULL and BOT has one.
			if existingNameEN == nil && nameEN != "" {
				if _, err := tx.Exec(ctx,
					`UPDATE public_holidays SET name_en = $1 WHERE id = $2`,
					nameEN, existingID); err != nil {
					return zero, err
				}
				result.Updated++
			} else {
				result.Skipped++
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return zero, err
	}
	s.aud.Log(ctx, audit.Entry{
		ActorID: &actor, Action: "holiday.sync_bot", Entity: "holiday",
		Note: fmt.Sprintf("year=%d fetched=%d inserted=%d updated=%d skipped=%d",
			year, result.Fetched, result.Inserted, result.Updated, result.Skipped),
	})
	return result, nil
}

// fetchBOTHolidays calls the BOT Open API and returns the raw rows for `year`.
// Uses a bounded 15s timeout so a hung upstream can't wedge the request. The
// API key is never logged.
func (s *HolidayService) fetchBOTHolidays(ctx context.Context, year int) ([]botHolidayRow, error) {
	base := strings.TrimRight(s.botAPIBaseURL, "/")
	if base == "" {
		base = "https://gateway.api.bot.or.th/financial-institutions-holidays"
	}
	url := fmt.Sprintf("%s/?year=%d", base, year)

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &UserError{Status: 502, Msg: "สร้างคำขอไปยัง BOT API ไม่สำเร็จ"}
	}
	// BOT uses a bare API key in the Authorization header (no Bearer prefix),
	// per the OpenAPI spec's securitySchemes.name = "Authorization".
	req.Header.Set("Authorization", s.botAPIClientID)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &UserError{Status: 502, Msg: "ไม่สามารถติดต่อ BOT API ได้ (network / timeout)"}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &UserError{Status: 502, Msg: "อ่านผลลัพธ์จาก BOT API ไม่สำเร็จ"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UserError{Status: 502, Msg: fmt.Sprintf("BOT API ตอบกลับผิดปกติ (%d)", resp.StatusCode)}
	}

	// BOT actually wraps the array in an envelope: {"result": {"data": [...]}}
	// but historically has also returned the bare array shown in the OpenAPI
	// spec. Handle both to stay resilient.
	var envelope struct {
		Result struct {
			Data []botHolidayRow `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Result.Data) > 0 {
		return envelope.Result.Data, nil
	}
	var bare []botHolidayRow
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, &UserError{Status: 502, Msg: "รูปแบบข้อมูลจาก BOT API เปลี่ยน — โปรดตรวจสอบ"}
	}
	return bare, nil
}

// ---------------------------------------------------------------------------
// SyncFromBOTRange — call SyncFromBOT for every calendar year in [start, end]
// Used by the "prompt after term save" flow: a term spanning Aug 2026 –
// Jan 2027 needs holidays synced for both 2026 and 2027.
// ---------------------------------------------------------------------------

type SyncFromBOTYearResult struct {
	Year     int    `json:"year"`
	Fetched  int    `json:"fetched"`
	Inserted int    `json:"inserted"`
	Updated  int    `json:"updated"`
	Skipped  int    `json:"skipped"`
	// Error is populated only if the sync for THIS year failed. The loop keeps
	// going so a transient failure on year N doesn't lose the result for year N+1.
	Error string `json:"error,omitempty"`
}

type SyncFromBOTRangeResult struct {
	Years []SyncFromBOTYearResult `json:"years"`
	Total SyncFromBOTResult       `json:"total"`
}

// SyncFromBOTRange iterates years startYear..endYear (inclusive), calling
// SyncFromBOT for each in its own transaction. Per-year errors are captured in
// the result rather than short-circuiting so a partial success (e.g., 2026 OK,
// 2027 network flake) is still reported. Returns a hard error only when the
// range itself is invalid or when EVERY year failed with the same error class
// (missing key / bad request).
func (s *HolidayService) SyncFromBOTRange(ctx context.Context, actor uuid.UUID, startYear, endYear int) (SyncFromBOTRangeResult, error) {
	var zero SyncFromBOTRangeResult
	if startYear < 1900 || endYear > 3000 || startYear > endYear {
		return zero, Invalid("ช่วงปีไม่ถูกต้อง")
	}
	if endYear-startYear > 5 {
		return zero, Invalid("ช่วงปีกว้างเกินไป (จำกัดไว้ 5 ปีต่อคำขอ)")
	}

	out := SyncFromBOTRangeResult{Years: make([]SyncFromBOTYearResult, 0, endYear-startYear+1)}
	successCount := 0
	var firstErr error
	for y := startYear; y <= endYear; y++ {
		one, err := s.SyncFromBOT(ctx, actor, y)
		yr := SyncFromBOTYearResult{Year: y, Fetched: one.Fetched, Inserted: one.Inserted, Updated: one.Updated, Skipped: one.Skipped}
		if err != nil {
			yr.Error = err.Error()
			if firstErr == nil {
				firstErr = err
			}
		} else {
			successCount++
			out.Total.Fetched += one.Fetched
			out.Total.Inserted += one.Inserted
			out.Total.Updated += one.Updated
			out.Total.Skipped += one.Skipped
		}
		out.Years = append(out.Years, yr)
	}
	// If nothing succeeded, propagate the first error (preserves 503 for missing
	// key so the frontend can render a specific message). Otherwise return the
	// per-year breakdown; caller can decide how to surface partial failure.
	if successCount == 0 && firstErr != nil {
		return zero, firstErr
	}
	return out, nil
}
