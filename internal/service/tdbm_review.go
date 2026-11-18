package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/timeutil"
)

// One option is an original sitting, not a whole day. A lecture and a lab
// can be assigned independently, even when TDBM moves them to different days.
type TDBMMatchOption struct {
	SectionID    uuid.UUID `json:"section_id"`
	OriginalDate string    `json:"original_date"`
	HolidayName  string    `json:"holiday_name"`
	Kind         string    `json:"kind"`
	StartTime    string    `json:"start_time"`
	EndTime      string    `json:"end_time"`
	Blocked      bool      `json:"blocked"`
}

type tdbmQuerier interface {
	querier
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func tdbmMatchOptions(ctx context.Context, q tdbmQuerier, year, semester int) (map[uuid.UUID][]TDBMMatchOption, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT sx.id, to_char(h.holiday_date,'YYYY-MM-DD'),
		       (SELECT string_agg(DISTINCT h2.name_th, ', ' ORDER BY h2.name_th)
		          FROM public_holidays h2 WHERE h2.holiday_date = h.holiday_date),
		       sch.kind, to_char(sch.start_time,'HH24:MI'), to_char(sch.end_time,'HH24:MI'),
		       EXISTS (SELECT 1 FROM work_logs wl JOIN ta_request_assignments a ON a.id = wl.assignment_id
		               WHERE a.section_id = sx.id AND wl.work_date = h.holiday_date
		                 AND wl.activity = sch.kind AND wl.status IN ('submitted','approved'))
		FROM teaching_courses tc
		JOIN academic_terms term ON term.id = tc.term_id
		JOIN sections sx ON sx.teaching_course_id = tc.id
		JOIN public_holidays h ON h.holiday_date BETWEEN `+CourseStartSQL("tc")+` AND `+CourseEndSQL("tc")+`
		JOIN section_schedules sch ON sch.section_id = sx.id
		 AND sch.day_of_week = EXTRACT(DOW FROM h.holiday_date)::int
		 AND (h.start_time IS NULL OR (sch.start_time < h.end_time AND sch.end_time > h.start_time))
		WHERE term.academic_year = $1 AND term.semester = $2 AND tc.exported_at IS NULL
		 AND NOT EXISTS (SELECT 1 FROM makeup_schedules m WHERE m.section_id = sx.id
		                  AND m.original_date = h.holiday_date AND m.kind = sch.kind)
		ORDER BY 1, 2, 5`, year, semester)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]TDBMMatchOption{}
	for rows.Next() {
		var o TDBMMatchOption
		if err := rows.Scan(&o.SectionID, &o.OriginalDate, &o.HolidayName, &o.Kind, &o.StartTime, &o.EndTime, &o.Blocked); err != nil {
			return nil, err
		}
		out[o.SectionID] = append(out[o.SectionID], o)
	}
	return out, rows.Err()
}

type TDBMMatchInput struct {
	AcademicYear int             `json:"academic_year"`
	Semester     int             `json:"semester"`
	Items        []TDBMMatchItem `json:"items"`
}

type TDBMMatchItem struct {
	ExtraClassID int    `json:"extra_class_id"`
	OriginalDate string `json:"original_date"`
	Kind         string `json:"kind"`
}

// MatchMakeups commits the entire review together. Re-read under section
// locks; an old browser tab must never overwrite a lecturer or another sync.
func (s *TDBMService) MatchMakeups(ctx context.Context, actor uuid.UUID, in TDBMMatchInput) (int, error) {
	if in.AcademicYear < 1 || in.Semester < 1 || len(in.Items) == 0 || len(in.Items) > 100 {
		return 0, Invalid("เลือกคาบที่ต้องการจับคู่ 1–100 รายการ และระบุภาคเรียน")
	}
	seen := map[int]bool{}
	for _, item := range in.Items {
		if _, err := timeutil.ParseDate(item.OriginalDate); err != nil || item.ExtraClassID <= 0 || seen[item.ExtraClassID] || (item.Kind != "lecture" && item.Kind != "lab") {
			return 0, Invalid("รายการจับคู่ไม่ถูกต้องหรือเลือกรายการ TDBM ซ้ำ")
		}
		seen[item.ExtraClassID] = true
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Lock sections in a stable order, shared with the automatic writer.
	ids := make([]int, 0, len(in.Items))
	for id := range seen {
		ids = append(ids, id)
	}
	rows, err := tx.Query(ctx, `SELECT sx.id FROM sections sx WHERE sx.id IN (
		SELECT section_id FROM tdbm_extra_teachings WHERE extra_class_id = ANY($1)
		 AND academic_year = $2 AND semester = $3) ORDER BY sx.id FOR UPDATE`, ids, in.AcademicYear, in.Semester)
	if err != nil {
		return 0, err
	}
	lockedSections := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		lockedSections[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	options, err := tdbmMatchOptions(ctx, tx, in.AcademicYear, in.Semester)
	if err != nil {
		return 0, err
	}
	items := append([]TDBMMatchItem(nil), in.Items...)
	sort.Slice(items, func(i, j int) bool { return items[i].ExtraClassID < items[j].ExtraClassID })
	used := map[string]bool{}
	for _, item := range items {
		var c tdbmCandidate
		var courseID uuid.UUID
		var applied *uuid.UUID
		var status string
		err := tx.QueryRow(ctx, `SELECT t.extra_class_id, t.section_id, to_char(t.class_date,'YYYY-MM-DD'),
		 to_char(t.start_time,'HH24:MI'), to_char(t.end_time,'HH24:MI'), sx.teaching_course_id,
		 t.applied_makeup_id, COALESCE(t.opt_status,'')
		 FROM tdbm_extra_teachings t JOIN sections sx ON sx.id = t.section_id
		 WHERE t.extra_class_id = $1 AND t.academic_year = $2 AND t.semester = $3 FOR UPDATE OF t`,
			item.ExtraClassID, in.AcademicYear, in.Semester).Scan(&c.ExtraClassID, &c.SectionID, &c.ClassDate, &c.StartTime, &c.EndTime, &courseID, &applied, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, Conflict("ไม่พบรายการหรือยังไม่ได้จับคู่กลุ่มเรียน กรุณาโหลดข้อมูลใหม่")
		}
		if err != nil {
			return 0, err
		}
		if applied != nil || status == "C" || !lockedSections[c.SectionID] {
			return 0, Conflict("รายการถูกจับคู่หรือยกเลิกแล้ว กรุณาโหลดข้อมูลใหม่")
		}
		key := fmt.Sprintf("%s/%s/%s", c.SectionID, item.OriginalDate, item.Kind)
		valid := false
		for _, o := range options[c.SectionID] {
			if o.OriginalDate == item.OriginalDate && o.Kind == item.Kind && !o.Blocked {
				valid = true
			}
		}
		if !valid || used[key] {
			return 0, Conflict("คาบวันหยุดเดิมไม่ว่าง ไม่มีในตารางเรียน ถูกล็อก หรือมีชั่วโมงที่ส่งแล้ว กรุณาโหลดข้อมูลใหม่")
		}
		used[key] = true
		if entryDurationMins(c) <= 0 {
			return 0, Invalid("รายการ TDBM ไม่มีช่วงเวลาสอนที่ถูกต้อง กรุณาตรวจสอบต้นทาง")
		}
		for _, d := range []string{item.OriginalDate, c.ClassDate} {
			if err := assertDateInCourseWindow(ctx, tx, courseID, d, "วันที่สอน"); err != nil {
				return 0, err
			}
			if err := assertMonthNotExportedForCourse(ctx, tx, courseID, d); err != nil {
				return 0, err
			}
		}
		n, err := s.insertTDBMMakeup(ctx, tx, c.SectionID, unresolvedPeriod{SectionID: c.SectionID, HolidayDate: item.OriginalDate, Kind: item.Kind}, c, &actor)
		if err != nil {
			return 0, err
		}
		if n != 1 {
			return 0, Conflict("มีผู้จับคู่คาบนี้แล้ว กรุณาโหลดข้อมูลใหม่")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(items), nil
}

// Both the makeup and every alias of the TDBM sitting are linked in the same
// transaction. A failed write or process crash cannot leave a consumed sitting
// eligible for a second holiday on the next sweep.
func (s *TDBMService) insertTDBMMakeup(ctx context.Context, tx pgx.Tx, sectionID uuid.UUID, p unresolvedPeriod, c tdbmCandidate, actor *uuid.UUID) (int, error) {
	var occupied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tdbm_extra_teachings
	 WHERE section_id = $1 AND class_date = $2::date AND start_time = $3::time AND end_time = $4::time
	 AND applied_makeup_id IS NOT NULL)`, sectionID, c.ClassDate, c.StartTime, c.EndTime).Scan(&occupied); err != nil {
		return 0, err
	}
	if occupied {
		return 0, nil
	}
	id := uuid.New()
	note := fmt.Sprintf("นำเข้าอัตโนมัติจาก TDBM (extra_class_id=%d)", c.ExtraClassID)
	action := "makeup.auto_fill_from_tdbm"
	if actor != nil {
		note = fmt.Sprintf("เจ้าหน้าที่จับคู่จาก TDBM (extra_class_id=%d)", c.ExtraClassID)
		action = "makeup.match_from_tdbm"
	}
	tag, err := tx.Exec(ctx, `INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, start_time, end_time, note, kind)
	 VALUES ($1,$2,$3::date,$4::date,$5,$6,$7,$8) ON CONFLICT (section_id, original_date, kind) DO NOTHING`,
		id, sectionID, p.HolidayDate, c.ClassDate, c.StartTime, c.EndTime, note, p.Kind)
	if err != nil || tag.RowsAffected() == 0 {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM work_logs wl USING ta_request_assignments a
	 WHERE a.id = wl.assignment_id AND a.section_id = $1 AND wl.work_date = $2::date
	 AND wl.activity = $3 AND wl.status = 'draft'`, sectionID, p.HolidayDate, p.Kind); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tdbm_extra_teachings SET applied_makeup_id = $1
	 WHERE section_id = $2 AND class_date = $3::date AND start_time = $4::time AND end_time = $5::time
	 AND applied_makeup_id IS NULL AND opt_status <> 'C'`, id, sectionID, c.ClassDate, c.StartTime, c.EndTime); err != nil {
		return 0, err
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: actor, Action: action, Entity: "section", EntityID: sectionID.String(),
		Note: fmt.Sprintf("original=%s kind=%s makeup=%s %s-%s (extra_class_id=%d)", p.HolidayDate, p.Kind, c.ClassDate, c.StartTime, c.EndTime, c.ExtraClassID)}); err != nil {
		return 0, err
	}
	return 1, nil
}
