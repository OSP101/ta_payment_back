package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// worklog_edit_batch.go turns a staff correction into something anyone can audit
// afterwards.
//
// StaffUpsert and StaffDelete still exist and still do the validating; what they
// never carried is the part a finance office actually asks for six months later:
// WHY were these hours changed, who said so, and what were they looking at. The
// lecturer who approved the original hours was not even told.
//
// A batch answers all three at once — one reason, one password, up to three
// images, applied to one (TA, course, month) — and notifies both the TA and every
// lecturer on the course.

const (
	editReasonMinLen = 10
	maxEvidenceFiles = 3
)

// EditChange is one row the officer wants changed. Action is "update" or
// "delete"; After is ignored for a delete.
type EditChange struct {
	WorkLogID uuid.UUID `json:"work_log_id"`
	Action    string    `json:"action"`
	After     WorkLog   `json:"after"`
}

// EditEvidence is one already-stored image, handed over by the handler after it
// has sniffed and saved the upload.
type EditEvidence struct {
	StorageKey string
	Filename   string
	Size       int64
	MIME       string
}

// EditBatchInput is everything the endpoint collected.
type EditBatchInput struct {
	TeachingCourseID uuid.UUID
	TAID             uuid.UUID
	YearMonth        string // "2026-07" — Gregorian, matching work_logs.work_date
	Reason           string
	Password         string
	Changes          []EditChange
	Evidence         []EditEvidence
}

// EditBatchResult reports what actually happened, which is not always what was
// asked for — see the partial-application note in ApplyStaffEditBatch.
type EditBatchResult struct {
	BatchID uuid.UUID `json:"batch_id"`
	Applied int       `json:"applied"`
	Failed  int       `json:"failed"`
	// Errors are the per-row refusals, already in Thai, so the officer can see
	// which correction the validator would not accept and why.
	Errors []string `json:"errors,omitempty"`
}

// ApplyStaffEditBatch validates the officer, applies each change through the
// normal staff write path, then records the batch.
//
// PARTIAL APPLICATION IS DELIBERATE. Each row goes through StaffUpsert, which
// enforces the daily cap, the weekly quota, the own-class rule and the overlap
// rule; a batch of five where the third breaks a cap should not silently discard
// the two that were fine, and it must not bypass the validator to keep them
// together either. So every row is attempted, the successes stand, and the
// recorded batch lists exactly what went through — never what was requested.
// The caller is told the count and the refusals.
func (s *WorkLogService) ApplyStaffEditBatch(
	ctx context.Context, actor uuid.UUID, in EditBatchInput,
) (*EditBatchResult, error) {
	if len(in.Changes) == 0 {
		return nil, Invalid("ไม่มีรายการที่แก้ไข")
	}
	if len([]rune(strings.TrimSpace(in.Reason))) < editReasonMinLen {
		return nil, Invalid(fmt.Sprintf(
			"ต้องระบุเหตุผลอย่างน้อย %d ตัวอักษร — เหตุผลนี้จะถูกส่งให้อาจารย์และทีเอ", editReasonMinLen))
	}
	if len(in.Evidence) > maxEvidenceFiles {
		return nil, Invalid(fmt.Sprintf("แนบรูปได้ไม่เกิน %d รูป", maxEvidenceFiles))
	}
	// Password last among the cheap checks: no reason to spend a bcrypt compare
	// on a request that was malformed anyway.
	if err := VerifyUserPassword(ctx, s.pool, actor, in.Password); err != nil {
		return nil, err
	}

	// Snapshot BEFORE touching anything. Once StaffUpsert has run there is no way
	// back to the old values, and "what did it say before" is the first question
	// asked about any correction.
	before := map[uuid.UUID]map[string]any{}
	ids := make([]uuid.UUID, 0, len(in.Changes))
	for _, c := range in.Changes {
		ids = append(ids, c.WorkLogID)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT wl.id, wl.work_date::text, wl.start_time::text, wl.end_time::text,
		       wl.hours, wl.activity::text, COALESCE(wl.note,''), wl.status::text,
		       COALESCE(sec.sec_no,'')
		FROM work_logs wl
		JOIN ta_request_assignments a ON a.id = wl.assignment_id
		JOIN sections sec ON sec.id = a.section_id
		WHERE wl.id = ANY($1) AND a.ta_id = $2 AND sec.teaching_course_id = $3`,
		ids, in.TAID, in.TeachingCourseID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		var date, st, en, act, note, status, secNo string
		var hrs float64
		if err := rows.Scan(&id, &date, &st, &en, &hrs, &act, &note, &status, &secNo); err != nil {
			rows.Close()
			return nil, err
		}
		before[id] = map[string]any{
			"work_date": date, "start_time": st[:5], "end_time": en[:5],
			"hours": hrs, "activity": act, "note": note, "status": status, "sec_no": secNo,
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Every id must belong to THIS TA on THIS course. Without it the endpoint is
	// an arbitrary-row editor that happens to take a course id in its path.
	for _, c := range in.Changes {
		if _, ok := before[c.WorkLogID]; !ok {
			return nil, Invalid("มีรายการที่ไม่ได้อยู่ในเดือน/วิชา/ทีเอนี้ — ยกเลิกทั้งชุด")
		}
	}

	res := &EditBatchResult{}
	applied := make([]map[string]any, 0, len(in.Changes))
	for _, c := range in.Changes {
		var err error
		switch c.Action {
		case "delete":
			err = s.StaffDelete(ctx, actor, true, c.WorkLogID)
		case "update":
			w := c.After
			w.ID = c.WorkLogID
			_, err = s.StaffUpsert(ctx, actor, true, w)
		default:
			err = Invalid("action ต้องเป็น update หรือ delete")
		}
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors,
				fmt.Sprintf("%s: %s", before[c.WorkLogID]["work_date"], errText(err)))
			continue
		}
		res.Applied++
		entry := map[string]any{
			"work_log_id": c.WorkLogID,
			"action":      c.Action,
			"before":      before[c.WorkLogID],
		}
		if c.Action == "update" {
			entry["after"] = map[string]any{
				"work_date": c.After.WorkDate, "start_time": c.After.StartTime,
				"end_time": c.After.EndTime, "hours": c.After.Hours,
				"activity": c.After.Activity, "note": c.After.Note,
			}
		}
		applied = append(applied, entry)
	}

	if res.Applied == 0 {
		// Nothing changed, so there is nothing to explain and no evidence worth
		// keeping. Report the refusals instead of writing an empty batch.
		return nil, Invalid("แก้ไขไม่สำเร็จสักรายการ — " + strings.Join(res.Errors, " · "))
	}

	changesJSON, err := json.Marshal(applied)
	if err != nil {
		return nil, err
	}
	// The batch row, its evidence and the audit entry are one record of one act:
	// a batch saved without its evidence rows, or without the audit entry, is a
	// correction to approved hours that cannot be explained afterwards.
	var batchID uuid.UUID
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if err := tx.QueryRow(ctx, `
		INSERT INTO worklog_edit_batches
		    (teaching_course_id, ta_id, year_month, actor_id, reason, changes)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb) RETURNING id`,
		in.TeachingCourseID, in.TAID, in.YearMonth, actor,
		strings.TrimSpace(in.Reason), string(changesJSON)).Scan(&batchID); err != nil {
		return nil, err
	}
	res.BatchID = batchID

	for _, f := range in.Evidence {
		if _, err := tx.Exec(ctx, `
			INSERT INTO worklog_edit_files (batch_id, storage_key, filename, size_bytes, mime)
			VALUES ($1,$2,$3,$4,$5)`,
			batchID, f.StorageKey, f.Filename, f.Size, f.MIME); err != nil {
			return nil, err
		}
	}

	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actor, Action: "worklog.staff_edit_batch",
		Entity: "worklog_edit_batch", EntityID: batchID.String(),
		Note: fmt.Sprintf("%s — แก้ %d รายการ, แนบ %d รูป: %s",
			in.YearMonth, res.Applied, len(in.Evidence), strings.TrimSpace(in.Reason)),
		After: applied,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// After the commit: notifications reach people and cannot be taken back if
	// the transaction were to fail behind them.
	s.notifyEditBatch(ctx, in, res.Applied, len(in.Evidence))
	return res, nil
}

// notifyEditBatch tells the TA and every lecturer on the course.
//
// The lecturer is the half that was missing: they approved these hours, and a
// correction made afterwards silently rewrites a decision they signed for.
func (s *WorkLogService) notifyEditBatch(ctx context.Context, in EditBatchInput, applied, files int) {
	if s.notify == nil {
		return
	}
	var code, nameTH string
	_ = s.pool.QueryRow(ctx,
		`SELECT code, COALESCE(name_th,'') FROM teaching_courses WHERE id=$1`,
		in.TeachingCourseID).Scan(&code, &nameTH)

	title := "เจ้าหน้าที่แก้ไขบันทึกเวลา " + code
	body := fmt.Sprintf("%s %s · เดือน %s — แก้ไข %d รายการ\nเหตุผล: %s",
		code, nameTH, in.YearMonth, applied, strings.TrimSpace(in.Reason))
	if files > 0 {
		body += fmt.Sprintf("\n(แนบหลักฐาน %d รูป)", files)
	}

	s.notify.Send(ctx, in.TAID, title, body,
		"/ta/courses/"+in.TeachingCourseID.String()+"/worklog")

	rows, err := s.pool.Query(ctx,
		`SELECT lecturer_id FROM teaching_lecturers WHERE teaching_course_id = $1`,
		in.TeachingCourseID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return
		}
		s.notify.Send(ctx, id, title, body,
			"/lecturer/courses/"+in.TeachingCourseID.String())
	}
}

// errText pulls the Thai message out of a UserError, falling back to the raw
// error so a programming fault is never rendered as an empty string.
func errText(err error) string {
	var ue *UserError
	if errors.As(err, &ue) {
		return ue.Msg
	}
	return err.Error()
}

// EditBatchSummary is one past correction, as the reviewer needs to read it.
type EditBatchSummary struct {
	ID        uuid.UUID       `json:"id"`
	YearMonth string          `json:"year_month"`
	ActorName string          `json:"actor_name"`
	Reason    string          `json:"reason"`
	Changes   json.RawMessage `json:"changes"`
	Files     []EditBatchFile `json:"files"`
	CreatedAt string          `json:"created_at"`
}

type EditBatchFile struct {
	Filename string `json:"filename"`
	URL      string `json:"url"`
}

// ListEditBatches returns past corrections for a (course, TA), newest first.
// yearMonth is optional; empty means every month.
func (s *WorkLogService) ListEditBatches(
	ctx context.Context, tcID, taID uuid.UUID, yearMonth string,
) ([]EditBatchSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT b.id, b.year_month,
		       COALESCE(u.first_name || ' ' || u.last_name, ''),
		       b.reason, b.changes,
		       TO_CHAR(b.created_at, 'YYYY-MM-DD"T"HH24:MI:SSTZH:TZM')
		FROM worklog_edit_batches b
		JOIN users u ON u.id = b.actor_id
		WHERE b.teaching_course_id = $1 AND b.ta_id = $2
		  AND ($3 = '' OR b.year_month = $3)
		ORDER BY b.created_at DESC`, tcID, taID, yearMonth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EditBatchSummary{}
	for rows.Next() {
		var b EditBatchSummary
		if err := rows.Scan(&b.ID, &b.YearMonth, &b.ActorName, &b.Reason,
			&b.Changes, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.Files = []EditBatchFile{}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Files in one pass rather than a query per batch — the history panel shows
	// every batch at once and N+1 here is a page load with a dozen round trips.
	for i := range out {
		frows, ferr := s.pool.Query(ctx,
			`SELECT filename, storage_key FROM worklog_edit_files
			  WHERE batch_id = $1 ORDER BY created_at`, out[i].ID)
		if ferr != nil {
			continue
		}
		for frows.Next() {
			var name, key string
			if err := frows.Scan(&name, &key); err == nil {
				out[i].Files = append(out[i].Files, EditBatchFile{
					Filename: name, URL: "/api/v1/worklog-edit-files/" + key,
				})
			}
		}
		frows.Close()
	}
	return out, nil
}
