package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const manualMakeupChanged = "ข้อมูลวันชดเชยเปลี่ยนแล้ว กรุณาโหลดข้อมูลใหม่และตรวจสอบวันที่กับเวลาก่อนบันทึก"

// The client shows the original date but submits the displayed actual date.
// Re-read the chosen sitting before writing; a stale preview must not silently
// redirect an entry to dates/times the TA has not seen. This is also checked
// inside the audited write transaction, where FOR SHARE holds the sitting.
func validateManualMakeupChoice(ctx context.Context, q querier, ac *assignmentContext, w WorkLog, lock bool) error {
	if w.OriginalWorkDate == nil && w.MakeupID == nil {
		return nil
	}
	if w.OriginalWorkDate == nil || w.MakeupID == nil || (w.Activity != "lecture" && w.Activity != "lab") {
		return Invalid(manualMakeupChanged)
	}
	if _, err := time.Parse("2006-01-02", *w.OriginalWorkDate); err != nil {
		return Invalid(manualMakeupChanged)
	}
	sql := `SELECT to_char(m.makeup_date,'YYYY-MM-DD'),
 COALESCE(to_char(m.start_time,'HH24:MI'),to_char(g.start_time,'HH24:MI')),
 COALESCE(to_char(m.end_time,'HH24:MI'),to_char(g.end_time,'HH24:MI'))
 FROM makeup_schedules m LEFT JOIN LATERAL (
 SELECT min(start_time) start_time,min(end_time) end_time FROM section_schedules
 WHERE section_id=m.section_id AND kind=m.kind AND day_of_week=extract(dow FROM m.original_date)::int
 HAVING count(DISTINCT(start_time,end_time))=1) g ON TRUE
 WHERE m.id=$1 AND m.section_id=$2 AND m.original_date=$3::date AND m.kind=$4 AND NOT m.waived`
	if lock {
		sql += " FOR SHARE OF m"
	}
	var date string
	var start, end *string
	err := q.QueryRow(ctx, sql, w.MakeupID, ac.SectionID, *w.OriginalWorkDate, w.Activity).Scan(&date, &start, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conflict(manualMakeupChanged)
	}
	if err != nil {
		return err
	}
	sm, ok1 := parseHM(w.StartTime)
	em, ok2 := parseHM(w.EndTime)
	if start == nil || end == nil {
		return Conflict(manualMakeupChanged)
	}
	ms, ok3 := parseHM(*start)
	me, ok4 := parseHM(*end)
	if date != w.WorkDate || !ok1 || !ok2 || !ok3 || !ok4 || em <= sm || sm < ms || em > me {
		return Conflict(manualMakeupChanged)
	}
	return nil
}

func (m makeupIndex) originalDates(date, kind string) ([]time.Time, bool) {
	if m.byOriginalKind != nil {
		ds := m.byOriginalKind[date][kind]
		return ds, len(ds) > 0
	}
	ds, ok := m.byOriginal[date]
	return ds, ok
}

func (m makeupIndex) kindWaived(date, kind string) bool {
	if m.waivedKind != nil {
		return m.waivedKind[date][kind]
	}
	return m.waived[date]
}
