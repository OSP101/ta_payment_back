package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// Lecturer corrections during review (09/2026).
//
// Sending a month back asks the TA to fix it, which is the wrong tool when the
// lecturer already knows the right answer — "this lab ran 13:00–15:00, not
// 13:00–16:00", or "this day did not happen". The lecturer can now correct a
// submitted row directly, or cut it, with a written reason the TA sees and the
// record keeps (work_log_lecturer_changes).
//
// Only rows still awaiting review can be touched here. An approved row is a
// signed record; changing one stays on the staff path, with its password
// step-up (StaffUpsert / EditStepUp).
//
// A co-taught sitting is one real session written against every section it
// served, and the review screen shows it once. A correction therefore reaches
// every copy, in one transaction, so the sections can never disagree about
// when the session ran.

// LecturerAdjustInput is a correction to one submitted sitting. Only the clock
// and hours change; date and activity stay the TA's — a different day or a
// different kind of work is a different claim, which is what cut + the TA
// re-entering it is for.
type LecturerAdjustInput struct {
	LogID     uuid.UUID
	StartTime string
	EndTime   string
	Hours     float64
	Reason    string
}

// WorkLogChange is one row of work_log_lecturer_changes, for the lecturer's
// review screen and the TA's own worklog page.
type WorkLogChange struct {
	ID           int64           `json:"id"`
	AssignmentID uuid.UUID       `json:"assignment_id"`
	WorkLogID    uuid.UUID       `json:"work_log_id"`
	Action       string          `json:"action"` // "edit" | "cut"
	WorkDate     string          `json:"work_date"`
	Before       json.RawMessage `json:"before"`
	After        json.RawMessage `json:"after,omitempty"`
	Reason       string          `json:"reason"`
	ActorName    string          `json:"actor_name"`
	ActorRole    string          `json:"actor_role"`
	At           time.Time       `json:"at"`
}

// changeSnapshot is what before/after hold: enough to redraw the row.
type changeSnapshot struct {
	WorkDate   string  `json:"work_date"`
	StartTime  string  `json:"start_time"`
	EndTime    string  `json:"end_time"`
	Hours      float64 `json:"hours"`
	Activity   string  `json:"activity"`
	ParentKind *string `json:"parent_kind,omitempty"`
	Note       *string `json:"note,omitempty"`
	SecNo      string  `json:"sec_no"`
}

type sittingCopy struct {
	WorkLog
	SecNo string
	Track string
}

// sittingCopies returns the submitted row logID and every other submitted copy
// of the same co-taught sitting (same TA, same request, same group, same date
// and clock). Regular-track copies first: that is the side rule B2 bills.
func (s *WorkLogService) sittingCopies(ctx context.Context, logID uuid.UUID) ([]sittingCopy, error) {
	rows, err := s.pool.Query(ctx, `
		WITH me AS (
		    SELECT wl.id, wl.work_date, wl.start_time, wl.end_time, wl.status,
		           a.ta_id, a.request_id, a.cotaught_group
		    FROM work_logs wl
		    JOIN ta_request_assignments a ON a.id = wl.assignment_id
		    WHERE wl.id = $1
		)
		SELECT wl.id, wl.assignment_id, TO_CHAR(wl.work_date,'YYYY-MM-DD'),
		       LEFT(wl.start_time::text, 5), LEFT(wl.end_time::text, 5), wl.hours,
		       wl.activity, wl.parent_kind, wl.room, wl.note, wl.status::text,
		       sec.sec_no, sec.track::text
		FROM me
		JOIN work_logs wl ON wl.work_date = me.work_date
		                 AND wl.start_time = me.start_time
		                 AND wl.end_time = me.end_time
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		WHERE wl.id = me.id
		   OR (me.cotaught_group IS NOT NULL
		       AND a.ta_id = me.ta_id AND a.request_id = me.request_id
		       AND a.cotaught_group = me.cotaught_group
		       AND wl.status = me.status)
		ORDER BY (sec.track = 'regular') DESC, sec.sec_no`, logID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sittingCopy
	for rows.Next() {
		var c sittingCopy
		if err := rows.Scan(&c.ID, &c.AssignmentID, &c.WorkDate, &c.StartTime, &c.EndTime, &c.Hours,
			&c.Activity, &c.ParentKind, &c.Room, &c.Note, &c.Status, &c.SecNo, &c.Track); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, Invalid("ไม่พบรายการที่ต้องการแก้ไข")
	}
	return out, nil
}

// reviewCorrectionTarget loads a sitting for correction and checks the actor
// may make it: a lecturer of the course (or staff/admin), on a row still
// awaiting review, with a reason.
func (s *WorkLogService) reviewCorrectionTarget(ctx context.Context, actor uuid.UUID, privileged bool, logID uuid.UUID, reason string) ([]sittingCopy, *assignmentContext, error) {
	copies, err := s.sittingCopies(ctx, logID)
	if err != nil {
		return nil, nil, err
	}
	ac, err := loadAssignmentContext(ctx, s.pool, copies[0].AssignmentID)
	if err != nil {
		return nil, nil, err
	}
	if !privileged {
		owns, err := lecturerOwnsCourse(ctx, s.pool, actor, ac.TeachingCourseID)
		if err != nil {
			return nil, nil, err
		}
		if !owns {
			return nil, nil, ErrForbidden
		}
	}
	for _, c := range copies {
		if c.Status != "submitted" {
			return nil, nil, Invalid("แก้ไขหรือตัดออกได้เฉพาะรายการที่รอพิจารณา รายการนี้" + statusTH(c.Status))
		}
	}
	if len([]rune(strings.TrimSpace(reason))) < editReasonMinLen {
		return nil, nil, Invalid(fmt.Sprintf("กรุณาระบุเหตุผลอย่างน้อย %d ตัวอักษร เหตุผลจะแสดงให้ TA เห็น", editReasonMinLen))
	}
	if err := assertWorklogNotExported(ctx, s.pool, ac.TeachingCourseID, ac.TAID, copies[0].WorkDate); err != nil {
		return nil, nil, err
	}
	return copies, ac, nil
}

func statusTH(st string) string {
	switch st {
	case "approved":
		return "อนุมัติแล้ว"
	case "rejected":
		return "ถูกส่งกลับแล้ว"
	case "draft":
		return "ยังเป็นฉบับร่าง"
	}
	return "มีสถานะ " + st
}

func snapshotOf(c sittingCopy) changeSnapshot {
	return changeSnapshot{
		WorkDate: c.WorkDate, StartTime: hhmmPrefix(c.StartTime), EndTime: hhmmPrefix(c.EndTime),
		Hours: c.Hours, Activity: c.Activity, ParentKind: c.ParentKind, Note: c.Note, SecNo: c.SecNo,
	}
}

func actorRoleFor(privileged bool) string {
	if privileged {
		return "staff"
	}
	return "lecturer"
}

func (s *WorkLogService) recordChange(ctx context.Context, tx pgx.Tx, c sittingCopy, action string,
	before changeSnapshot, after *changeSnapshot, reason string, actor uuid.UUID, actorName, role string) error {
	b, err := json.Marshal(before)
	if err != nil {
		return err
	}
	var a []byte
	if after != nil {
		if a, err = json.Marshal(after); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO work_log_lecturer_changes
		    (assignment_id, work_log_id, action, work_date, before, after, reason, actor_id, actor_name, actor_role)
		VALUES ($1, $2, $3, $4::date, $5, $6, $7, $8, $9, $10)`,
		c.AssignmentID, c.ID, action, c.WorkDate, b, a, strings.TrimSpace(reason), actor, actorName, role)
	return err
}

// LecturerAdjust corrects the clock/hours of a submitted sitting, on every
// co-taught copy, and records why. The row stays submitted: the lecturer
// still approves it as a separate act.
func (s *WorkLogService) LecturerAdjust(ctx context.Context, actor uuid.UUID, privileged bool, in LecturerAdjustInput) error {
	copies, ac, err := s.reviewCorrectionTarget(ctx, actor, privileged, in.LogID, in.Reason)
	if err != nil {
		return err
	}
	start, end := hhmmPrefix(strings.TrimSpace(in.StartTime)), hhmmPrefix(strings.TrimSpace(in.EndTime))
	if start == hhmmPrefix(copies[0].StartTime) && end == hhmmPrefix(copies[0].EndTime) &&
		in.Hours > copies[0].Hours-0.001 && in.Hours < copies[0].Hours+0.001 {
		return Invalid("ยังไม่ได้เปลี่ยนเวลาหรือจำนวนชั่วโมง")
	}
	ids := make([]uuid.UUID, len(copies))
	for i, c := range copies {
		ids[i] = c.ID
	}
	// Every copy passes the same rules a staff edit does, each against its own
	// section — the daily hour, weekly and term caps are per assignment. The
	// other copies are left out of the day's pay total so the sitting is not
	// priced beside its own old self.
	for _, c := range copies {
		w := c.WorkLog
		w.StartTime, w.EndTime, w.Hours = start, end, in.Hours
		cac := ac
		if c.AssignmentID != copies[0].AssignmentID {
			if cac, err = loadAssignmentContext(ctx, s.pool, c.AssignmentID); err != nil {
				return err
			}
		}
		if err := s.validateOnBehalfWrite(ctx, cac, w, onBehalfOpts{reviewOnly: true, sameSitting: ids}); err != nil {
			if len(copies) > 1 {
				return fmt.Errorf("sec %s: %w", c.SecNo, err)
			}
			return err
		}
	}

	actorName := personName(ctx, s.pool, actor)
	role := actorRoleFor(privileged)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, c := range copies {
		before := snapshotOf(c)
		after := before
		after.StartTime, after.EndTime, after.Hours = start, end, in.Hours
		// status = 'submitted' in the predicate: an approval racing this edit
		// must not have its signed-off row rewritten underneath it.
		tag, err := tx.Exec(ctx,
			`UPDATE work_logs SET start_time=$1::time, end_time=$2::time, hours=$3
			  WHERE id=$4 AND status='submitted'`, start, end, in.Hours, c.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return Conflict("รายการนี้เปลี่ยนสถานะระหว่างแก้ไข (อาจเพิ่งได้รับอนุมัติ) กรุณาโหลดใหม่แล้วลองอีกครั้ง")
		}
		if err := s.recordChange(ctx, tx, c, "edit", before, &after, in.Reason, actor, actorName, role); err != nil {
			return err
		}
		if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "worklog.review_edit",
			Entity: "work_log", EntityID: c.ID.String(), Before: before, After: after,
			Note: "เหตุผล: " + strings.TrimSpace(in.Reason)}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.notify != nil {
		t := s.notifyTarget(ctx, ac.TeachingCourseID)
		c := copies[0]
		s.notify.Send(ctx, ac.TAID,
			fmt.Sprintf("อาจารย์แก้ไขบันทึกเวลา %s วันที่ %s", t.Code, thaiLongDateISO(c.WorkDate)),
			fmt.Sprintf("%s ได้แก้ไขบันทึกเวลาปฏิบัติงานรายวิชา %s ของวันที่ %s จาก%s (%.2f ชั่วโมง) เป็น%s (%.2f ชั่วโมง) เนื่องจาก %s",
				actorName, t.Label(), thaiLongDateISO(c.WorkDate),
				thaiTimeRange(c.StartTime, c.EndTime), c.Hours, thaiTimeRange(start, end), in.Hours,
				strings.TrimSpace(in.Reason)),
			t.Link)
	}
	return nil
}

// LecturerCut removes a submitted sitting (every co-taught copy) from the
// claim, keeping its last state and the reason in work_log_lecturer_changes.
func (s *WorkLogService) LecturerCut(ctx context.Context, actor uuid.UUID, privileged bool, logID uuid.UUID, reason string) error {
	copies, ac, err := s.reviewCorrectionTarget(ctx, actor, privileged, logID, reason)
	if err != nil {
		return err
	}
	actorName := personName(ctx, s.pool, actor)
	role := actorRoleFor(privileged)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, c := range copies {
		before := snapshotOf(c)
		if err := s.recordChange(ctx, tx, c, "cut", before, nil, reason, actor, actorName, role); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM work_logs WHERE id=$1 AND status='submitted'`, c.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return Conflict("รายการนี้เปลี่ยนสถานะระหว่างดำเนินการ (อาจเพิ่งได้รับอนุมัติ) กรุณาโหลดใหม่แล้วลองอีกครั้ง")
		}
		if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "worklog.review_cut",
			Entity: "work_log", EntityID: c.ID.String(), Before: before,
			Note: "เหตุผล: " + strings.TrimSpace(reason)}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if s.notify != nil {
		t := s.notifyTarget(ctx, ac.TeachingCourseID)
		c := copies[0]
		s.notify.Send(ctx, ac.TAID,
			fmt.Sprintf("อาจารย์ตัดบันทึกเวลาออก %s วันที่ %s", t.Code, thaiLongDateISO(c.WorkDate)),
			fmt.Sprintf("%s ได้ตัดบันทึกเวลาปฏิบัติงานรายวิชา %s ของวันที่ %s %s (%.2f ชั่วโมง) ออกจากการเบิก เนื่องจาก %s",
				actorName, t.Label(), thaiLongDateISO(c.WorkDate),
				thaiTimeRange(c.StartTime, c.EndTime), c.Hours, strings.TrimSpace(reason)),
			t.Link)
	}
	return nil
}

// ListChanges returns the corrections made to one assignment's rows, newest
// first. Readable by whoever may read the assignment's work log — the TA
// themselves included, which is the point.
func (s *WorkLogService) ListChanges(ctx context.Context, actor, assignmentID uuid.UUID, privileged bool) ([]WorkLogChange, error) {
	if err := s.assertCanView(ctx, actor, assignmentID, privileged); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, assignment_id, work_log_id, action, TO_CHAR(work_date,'YYYY-MM-DD'),
		       before, after, reason, actor_name, actor_role, created_at
		FROM work_log_lecturer_changes
		WHERE assignment_id = $1
		ORDER BY created_at DESC, id DESC`, assignmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkLogChange{}
	for rows.Next() {
		var c WorkLogChange
		var after []byte
		if err := rows.Scan(&c.ID, &c.AssignmentID, &c.WorkLogID, &c.Action, &c.WorkDate,
			&c.Before, &after, &c.Reason, &c.ActorName, &c.ActorRole, &c.At); err != nil {
			return nil, err
		}
		if after != nil {
			c.After = after
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
