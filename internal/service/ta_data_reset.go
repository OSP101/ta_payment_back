package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"ta-payment-back/internal/audit"
)

// ResetTADataInput invalidates selected submissions, keeping old files in history.
// TermID identifies both semester and academic year and is mandatory for schedules.
type ResetTADataInput struct {
	Documents []string  `json:"documents"`
	Schedule  bool      `json:"schedule"`
	TermID    uuid.UUID `json:"term_id"`
	Reason    string    `json:"reason"`
}

func (in *ResetTADataInput) validate() error {
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Documents) == 0 && !in.Schedule {
		return Invalid("กรุณาเลือกข้อมูลที่ต้องการรีเซ็ต")
	}
	if len(in.Documents) > len(DocKinds) {
		return Invalid("รายการเอกสารไม่ถูกต้อง")
	}
	seen := map[string]bool{}
	for _, kind := range in.Documents {
		if !DocKinds[kind] || seen[kind] {
			return Invalid("รายการเอกสารไม่ถูกต้องหรือซ้ำกัน")
		}
		seen[kind] = true
	}
	if in.Schedule && in.TermID == uuid.Nil {
		return Invalid("กรุณาเลือกภาคเรียนและปีการศึกษาที่ต้องการรีเซ็ตตารางเรียน")
	}
	if in.Reason == "" || len([]rune(in.Reason)) > 1000 {
		return Invalid("กรุณาระบุเหตุผลที่รีเซ็ต ไม่เกิน 1,000 ตัวอักษร")
	}
	return nil
}

// ResetTAData changes all selections and their audit entry atomically. It uses
// the same locks as document uploads and timetable saves, and preserves exports.
func (s *WorkloadService) ResetTAData(ctx context.Context, actor, target uuid.UUID, in ResetTADataInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var allowed, isTA bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_roles WHERE user_id=$1 AND role IN ('admin','staff')),
 EXISTS (SELECT 1 FROM user_roles WHERE user_id=$2 AND role='ta')`, actor, target).Scan(&allowed, &isTA); err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	if !isTA {
		return Invalid("บัญชีนี้ไม่ใช่ผู้ช่วยสอน")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, target.String()); err != nil {
		return err
	}
	// Profile lock order matches Upload, SaveProfile and document review.
	if _, err := tx.Exec(ctx, `SELECT user_id FROM ta_profiles WHERE user_id=$1 FOR UPDATE`, target); err != nil {
		return err
	}
	before := map[string]any{}
	if in.Schedule {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM academic_terms WHERE id=$1)`, in.TermID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return Invalid("ไม่พบภาคเรียนและปีการศึกษาที่เลือก")
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 7))`, "ta_class_schedules/"+target.String()+"/"+in.TermID.String()); err != nil {
			return err
		}
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM submission_period_status st JOIN submission_periods sp ON sp.id=st.submission_period_id WHERE st.ta_id=$1 AND sp.term_id=$2 AND st.status IN ('exported','finance_sent'))`, target, in.TermID).Scan(&locked); err != nil {
			return err
		}
		if locked {
			return Invalid("เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายของภาคเรียนนี้แล้ว ไม่สามารถรีเซ็ตตารางเรียนได้")
		}
		snapshot, err := classScheduleSnapshot(ctx, tx, target, in.TermID)
		if err != nil {
			return err
		}
		before["schedule"] = snapshot
		if _, err := tx.Exec(ctx, `DELETE FROM ta_class_schedules WHERE user_id=$1 AND term_id=$2`, target, in.TermID); err != nil {
			return err
		}
	}
	if len(in.Documents) > 0 {
		var status string
		err := tx.QueryRow(ctx, `SELECT status::text FROM ta_profiles WHERE user_id=$1`, target).Scan(&status)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		before["profile_status"] = status
		rows, err := tx.Query(ctx, `UPDATE ta_documents SET superseded_at=NOW() WHERE user_id=$1 AND kind=ANY($2::text[]) AND superseded_at IS NULL RETURNING id`, target, in.Documents)
		if err != nil {
			return err
		}
		ids := []uuid.UUID{}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		before["document_ids"] = ids
		profileStatus := "needs_fix"
		// Keep an earlier creditor/profile correction open when resetting another attachment.
		if status == "rejected" {
			profileStatus = "rejected"
		}
		for _, kind := range in.Documents {
			if kind == "creditor_form" {
				profileStatus = "rejected"
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE ta_profiles SET status=$2::doc_status, reject_reason=$3, verified_at=NOW(), verified_by=$4 WHERE user_id=$1`, target, profileStatus, in.Reason, actor); err != nil {
			return err
		}
		if err := pauseDocumentRetention(ctx, tx, target); err != nil {
			return err
		}
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "ta_data.reset", Entity: "user", EntityID: target.String(), Note: in.Reason, Before: before, After: in}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
