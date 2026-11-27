package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"ta-payment-back/internal/audit"
)

type tdbmPeriod struct {
	section        uuid.UUID
	original, kind string
}
type tdbmMirrorEntry struct {
	id                                       int
	section                                  *uuid.UUID
	applied                                  *uuid.UUID
	status, original, kind, date, start, end string
	missing                                  bool
	reference                                bool
}

func (e tdbmMirrorEntry) eligible() bool {
	return !e.missing && !e.reference && tdbmMakeupEligible(e.status)
}
func (e tdbmMirrorEntry) period() tdbmPeriod {
	var id uuid.UUID
	if e.section != nil {
		id = *e.section
	}
	return tdbmPeriod{id, e.original, e.kind}
}
func (e tdbmMirrorEntry) identity() string {
	return fmt.Sprintf("%v/%s/%s/%s", e.period(), e.date, e.start, e.end)
}

type tdbmAppliedSitting struct {
	id               uuid.UUID
	period           tdbmPeriod
	date, start, end string
	waived           bool
}

func (m tdbmAppliedSitting) same(e tdbmMirrorEntry) bool {
	return !m.waived && m.period == e.period() && m.date == e.date && m.start == e.start && m.end == e.end
}

const (
	tdbmReconcileConflict = "ข้อมูล TDBM เปลี่ยน แต่ปรับคาบชดเชยไม่ได้ กรุณาตรวจสอบบันทึกเวลา งวดเบิกจ่าย และรายการชดเชยที่ซ้ำกัน"
	tdbmSourceConflict    = "TDBM มีวันหรือเวลาชดเชยหลายชุดสำหรับคาบเดิมเดียวกัน กรุณาตรวจสอบคำขอต้นทาง"
	tdbmSentConflict      = "คาบนี้มีบันทึกเวลาที่ส่งหรืออนุมัติแล้ว กรุณาตีกลับก่อนซิงก์การเปลี่ยนแปลง"
	tdbmExportConflict    = "คาบนี้อยู่ในงวดเบิกจ่ายที่ล็อกแล้ว ต้องปลดล็อกก่อนซิงก์การเปลี่ยนแปลง"
	tdbmWaivedConflict    = "คาบเดิมถูกระบุว่าไม่มีการชดเชยใน TA PAY กรุณาตรวจสอบก่อนใช้ข้อมูล TDBM"
	tdbmInvalidSitting    = "ข้อมูลคาบเดิมหรือเวลาชดเชยจาก TDBM ไม่ครบหรือไม่ถูกต้อง"
	tdbmDateConflict      = "วันที่คาบเดิมหรือวันชดเชยจาก TDBM อยู่นอกช่วงวันที่รายวิชา"
)

// A component connects source rows by their authoritative original period,
// previous applied row, and the original period occupied by that old row.
// This lets an old API pairing A→B, B→A be corrected atomically. Processing
// each old applied ID independently deadlocked logically on the other's row.
func (s *TDBMService) reconcileMakeups(ctx context.Context, year, semester int) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	const affectedSections = `SELECT section_id FROM tdbm_extra_teachings WHERE academic_year=$1 AND semester=$2
 UNION SELECT m.section_id FROM makeup_schedules m JOIN tdbm_extra_teachings t ON t.applied_makeup_id=m.id
 WHERE t.academic_year=$1 AND t.semester=$2`
	locks, err := tx.Query(ctx, `SELECT id FROM sections WHERE id IN (`+affectedSections+`) ORDER BY id FOR UPDATE`, year, semester)
	if err != nil {
		return 0, err
	}
	for locks.Next() {
	}
	err = locks.Err()
	locks.Close()
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT extra_class_id,section_id,applied_makeup_id,COALESCE(opt_status,''),
 COALESCE(source_original_date::text,''),COALESCE(source_original_kind,''),COALESCE(class_date::text,''),
 COALESCE(to_char(start_time,'HH24:MI:SS'),''),COALESCE(to_char(end_time,'HH24:MI:SS'),''),source_missing,source_reference
 FROM tdbm_extra_teachings WHERE academic_year=$1 AND semester=$2
 AND (source_class_id IS NOT NULL OR source_teaching_id IS NOT NULL OR source_missing OR source_original_date IS NOT NULL)
 ORDER BY extra_class_id FOR UPDATE`, year, semester)
	if err != nil {
		return 0, err
	}
	var entries []tdbmMirrorEntry
	for rows.Next() {
		var e tdbmMirrorEntry
		if err = rows.Scan(&e.id, &e.section, &e.applied, &e.status, &e.original, &e.kind, &e.date, &e.start, &e.end, &e.missing, &e.reference); err != nil {
			rows.Close()
			return 0, err
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	rows, err = tx.Query(ctx, `SELECT m.id,m.section_id,m.original_date::text,m.kind,COALESCE(m.makeup_date::text,''),
 COALESCE(to_char(m.start_time,'HH24:MI:SS'),''),COALESCE(to_char(m.end_time,'HH24:MI:SS'),''),m.waived
 FROM makeup_schedules m WHERE m.section_id IN (`+affectedSections+`) ORDER BY m.id FOR UPDATE`, year, semester)
	if err != nil {
		return 0, err
	}
	applied := map[uuid.UUID]tdbmAppliedSitting{}
	byPeriod := map[tdbmPeriod]tdbmAppliedSitting{}
	for rows.Next() {
		var m tdbmAppliedSitting
		if err = rows.Scan(&m.id, &m.period.section, &m.period.original, &m.period.kind, &m.date, &m.start, &m.end, &m.waived); err != nil {
			rows.Close()
			return 0, err
		}
		applied[m.id] = m
		byPeriod[m.period] = m
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	parent := make([]int, len(entries))
	for i := range parent {
		parent[i] = i
	}
	var root func(int) int
	root = func(i int) int {
		if parent[i] != i {
			parent[i] = root(parent[i])
		}
		return parent[i]
	}
	owners := map[tdbmPeriod]int{}
	connect := func(key tdbmPeriod, i int) {
		if key.section == uuid.Nil || key.original == "" {
			return
		}
		if j, ok := owners[key]; ok {
			parent[root(i)] = root(j)
		} else {
			owners[key] = i
		}
	}
	for i, e := range entries {
		connect(e.period(), i)
		if e.applied != nil {
			if old, ok := applied[*e.applied]; ok {
				connect(old.period, i)
			}
		}
	}
	components := map[int][]tdbmMirrorEntry{}
	var order []int
	for i, e := range entries {
		r := root(i)
		if _, ok := components[r]; !ok {
			order = append(order, r)
		}
		components[r] = append(components[r], e)
	}
	filled := 0
	for _, key := range order {
		group := components[key]
		ids := make([]int, 0, len(group))
		for _, e := range group {
			ids = append(ids, e.id)
		}
		if _, err = tx.Exec(ctx, `SAVEPOINT tdbm_component`); err != nil {
			return 0, err
		}
		n, applyErr := s.reconcileComponent(ctx, tx, group, applied, byPeriod)
		if applyErr != nil {
			var ue *UserError
			if !errors.As(applyErr, &ue) {
				return 0, applyErr
			}
			if _, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT tdbm_component`); err != nil {
				return 0, err
			}
			if _, err = tx.Exec(ctx, `UPDATE tdbm_extra_teachings SET reconciliation_error=$1 WHERE extra_class_id=ANY($2)`, ue.Msg, ids); err != nil {
				return 0, err
			}
		} else {
			filled += n
			if _, err = tx.Exec(ctx, `UPDATE tdbm_extra_teachings SET reconciliation_error=NULL WHERE extra_class_id=ANY($1)`, ids); err != nil {
				return 0, err
			}
		}
		if _, err = tx.Exec(ctx, `RELEASE SAVEPOINT tdbm_component`); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return filled, nil
}

func (s *TDBMService) reconcileComponent(ctx context.Context, tx pgx.Tx, entries []tdbmMirrorEntry, applied map[uuid.UUID]tdbmAppliedSitting, byPeriod map[tdbmPeriod]tdbmAppliedSitting) (int, error) {
	desired := map[tdbmPeriod]tdbmMirrorEntry{}
	existing := map[tdbmPeriod]tdbmAppliedSitting{}
	ids := make([]int, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.id)
		if e.applied != nil {
			if old, ok := applied[*e.applied]; ok {
				existing[old.period] = old
			} else {
				return 0, Conflict(tdbmReconcileConflict)
			}
		}
		if !e.eligible() {
			continue
		}
		if e.section == nil && e.applied == nil {
			continue
		}
		if e.section == nil || e.original == "" || (e.kind != "lecture" && e.kind != "lab") || e.date == "" || e.start == "" || e.end <= e.start {
			return 0, Conflict(tdbmInvalidSitting)
		}
		key := e.period()
		if other, ok := desired[key]; ok && other.identity() != e.identity() {
			return 0, Conflict(tdbmSourceConflict)
		}
		desired[key] = e
		if old, ok := byPeriod[key]; ok {
			existing[key] = old
		}
	}
	// Every owner must participate before an existing TDBM link is replaced.
	for _, old := range existing {
		var outside bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tdbm_extra_teachings WHERE applied_makeup_id=$1 AND NOT(extra_class_id=ANY($2)))`, old.id, ids).Scan(&outside); err != nil {
			return 0, err
		}
		if outside {
			return 0, Conflict(tdbmReconcileConflict)
		}
	}
	// Validate the full plan before changing any row. An identical local sitting
	// can be adopted even after submission/export: attaching provenance does not
	// change dates, hours, amounts or existing manual notes.
	keys := make([]tdbmPeriod, 0, len(existing)+len(desired))
	seen := map[tdbmPeriod]bool{}
	for k := range existing {
		seen[k] = true
		keys = append(keys, k)
	}
	for k := range desired {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	for _, key := range keys {
		old, hasOld := existing[key]
		next, hasNext := desired[key]
		if hasOld && hasNext && old.same(next) {
			continue
		}
		if hasOld && old.waived {
			return 0, Conflict(tdbmWaivedConflict)
		}
		if hasOld {
			if err := tdbmCanChangeSitting(ctx, tx, key, old.date, false); err != nil {
				return 0, err
			}
		}
		if hasNext {
			if err := tdbmCanChangeSitting(ctx, tx, key, next.date, true); err != nil {
				return 0, err
			}
		}
	}
	// Detach/re-attach source IDs atomically so swapped or shared legacy links
	// cannot keep an entry associated with the wrong original sitting.
	if _, err := tx.Exec(ctx, `UPDATE tdbm_extra_teachings SET applied_makeup_id=NULL WHERE extra_class_id=ANY($1)`, ids); err != nil {
		return 0, err
	}
	result := map[tdbmPeriod]uuid.UUID{}
	changed := 0
	for _, key := range keys {
		old, hasOld := existing[key]
		next, hasNext := desired[key]
		if hasOld && hasNext && old.same(next) {
			result[key] = old.id
			continue
		}
		if hasOld {
			// Only remove drafts from the old makeup window; unrelated lecture/lab
			// duties on the same day must survive a source correction.
			if _, err := tx.Exec(ctx, `DELETE FROM work_logs wl USING ta_request_assignments a WHERE a.id=wl.assignment_id
    AND a.section_id=$1 AND wl.work_date=NULLIF($2,'')::date AND wl.activity=$3 AND wl.status='draft'
    AND (NULLIF($4,'')::time IS NULL OR (wl.start_time<NULLIF($5,'')::time AND wl.end_time>NULLIF($4,'')::time))`, key.section, old.date, key.kind, old.start, old.end); err != nil {
				return 0, err
			}
		}
		if !hasNext {
			if _, err := tx.Exec(ctx, `DELETE FROM makeup_schedules WHERE id=$1`, old.id); err != nil {
				return 0, err
			}
		} else {
			id := old.id
			if hasOld {
				if _, err := tx.Exec(ctx, `UPDATE makeup_schedules SET makeup_date=$2::date,start_time=$3::time,end_time=$4::time WHERE id=$1`, id, next.date, next.start, next.end); err != nil {
					return 0, err
				}
			} else {
				id = uuid.New()
				if _, err := tx.Exec(ctx, `INSERT INTO makeup_schedules(id,section_id,original_date,kind,makeup_date,start_time,end_time,note)
      VALUES($1,$2,$3::date,$4,$5::date,$6::time,$7::time,$8)`, id, key.section, key.original, key.kind, next.date, next.start, next.end, fmt.Sprintf("นำเข้าอัตโนมัติจาก TDBM MariaDB (extra_class_id=%d)", next.id)); err != nil {
					return 0, err
				}
			}
			result[key] = id
			changed++
			if _, err := tx.Exec(ctx, `DELETE FROM work_logs wl USING ta_request_assignments a WHERE a.id=wl.assignment_id AND a.section_id=$1 AND wl.work_date=$2::date AND wl.activity=$3 AND wl.status='draft'`, key.section, key.original, key.kind); err != nil {
				return 0, err
			}
		}
		if s.aud != nil {
			if err := s.aud.LogTx(ctx, tx, audit.Entry{Action: "makeup.sync_from_tdbm", Entity: "section", EntityID: key.section.String(), Note: fmt.Sprintf("source_ids=%v original=%s kind=%s before=%s %s-%s after=%s %s-%s", ids, key.original, key.kind, old.date, old.start, old.end, next.date, next.start, next.end)}); err != nil {
				return 0, err
			}
		}
	}
	for _, e := range entries {
		if !e.eligible() || e.section == nil {
			continue
		}
		if id, ok := result[e.period()]; ok {
			if _, err := tx.Exec(ctx, `UPDATE tdbm_extra_teachings SET applied_makeup_id=$1 WHERE extra_class_id=$2`, id, e.id); err != nil {
				return 0, err
			}
			if s.aud != nil && (e.applied == nil || *e.applied != id) {
				if err := s.aud.LogTx(ctx, tx, audit.Entry{Action: "makeup.sync_from_tdbm", Entity: "section", EntityID: e.section.String(), Note: fmt.Sprintf("source_id=%d linked_makeup=%s original=%s kind=%s", e.id, id, e.original, e.kind)}); err != nil {
					return 0, err
				}
			}
		}
	}
	return changed, nil
}

func tdbmCanChangeSitting(ctx context.Context, tx pgx.Tx, key tdbmPeriod, date string, validateWindow bool) error {
	var course uuid.UUID
	var exported bool
	if err := tx.QueryRow(ctx, `SELECT tc.id,tc.exported_at IS NOT NULL FROM sections s JOIN teaching_courses tc ON tc.id=s.teaching_course_id WHERE s.id=$1`, key.section).Scan(&course, &exported); err != nil {
		return err
	}
	if exported {
		return Conflict(tdbmExportConflict)
	}
	for _, d := range []string{key.original, date} {
		if d == "" {
			continue
		}
		if err := assertMonthNotExportedForCourse(ctx, tx, course, d); err != nil {
			var ue *UserError
			if errors.As(err, &ue) {
				return Conflict(tdbmExportConflict)
			}
			return err
		}
		if validateWindow {
			if err := assertDateInCourseWindow(ctx, tx, course, d, "วันที่สอน"); err != nil {
				var ue *UserError
				if errors.As(err, &ue) {
					return Conflict(tdbmDateConflict)
				}
				return err
			}
		}
	}
	var sent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_logs wl JOIN ta_request_assignments a ON a.id=wl.assignment_id
 WHERE a.section_id=$1 AND wl.activity=$2 AND wl.work_date IN($3::date,NULLIF($4,'')::date) AND wl.status IN('submitted','approved'))`, key.section, key.kind, key.original, date).Scan(&sent); err != nil {
		return err
	}
	if sent {
		return Conflict(tdbmSentConflict)
	}
	return nil
}
