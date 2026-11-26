package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Lock the approval graph before inspecting progress. Besides preserving the
// reviewed inputs, FOR UPDATE on assignments serializes new work-log FK inserts
// against the merge. Source-course references are never relaxed here.
func existingMergeLockIDs(ctx context.Context, tx pgx.Tx, table, column string, ids []uuid.UUID) ([]uuid.UUID, error) {
	q := fmt.Sprintf(`SELECT id FROM %s WHERE %s=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, pgx.Identifier{table}.Sanitize(), pgx.Identifier{column}.Sanitize())
	rows, err := tx.Query(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func existingMergeApprovedTarget(ctx context.Context, tx pgx.Tx, target ExistingMergeCourse) (json.RawMessage, []uuid.UUID, []string, error) {
	blockers := []string{}
	sectionIDs := []uuid.UUID{}
	for _, sec := range target.Sections {
		sectionIDs = append(sectionIDs, sec.ID)
	}
	requestIDs, err := existingMergeLockIDs(ctx, tx, "ta_requests", "teaching_course_id", []uuid.UUID{target.ID})
	if err != nil {
		return nil, nil, nil, err
	}
	assignmentIDs, err := existingMergeLockIDs(ctx, tx, "ta_request_assignments", "request_id", requestIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	workloadIDs, err := existingMergeLockIDs(ctx, tx, "ta_workload_forms", "assignment_id", assignmentIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	periodIDs, err := existingMergeLockIDs(ctx, tx, "submission_period_status", "teaching_course_id", []uuid.UUID{target.ID})
	if err != nil {
		return nil, nil, nil, err
	}

	var pending, foreignAssignment, workStarted, signed, changedAfterApproval bool
	if err = tx.QueryRow(ctx, `SELECT
	 EXISTS(SELECT 1 FROM ta_requests WHERE id=ANY($1::uuid[]) AND status NOT IN ('approved','cancelled','rejected')),
	 EXISTS(SELECT 1 FROM ta_request_assignments a JOIN ta_requests r ON r.id=a.request_id JOIN sections sec ON sec.id=a.section_id
	        WHERE (r.teaching_course_id=$2 AND sec.teaching_course_id<>$2) OR (sec.teaching_course_id=$2 AND r.teaching_course_id<>$2)),
	 EXISTS(SELECT 1 FROM work_logs WHERE assignment_id=ANY($3::uuid[])),
	 EXISTS(SELECT 1 FROM submission_period_status WHERE id=ANY($4::uuid[]) AND
	        (status<>'pending' OR ta_signed_at IS NOT NULL OR lecturer_signed_at IS NOT NULL OR submitted_at IS NOT NULL OR exported_at IS NOT NULL OR finance_sent_at IS NOT NULL OR staff_reviewed_by IS NOT NULL)),
	 EXISTS(SELECT 1 FROM ta_request_calculation_snapshots WHERE request_id=ANY($1::uuid[])
	        AND inputs->'request'->>'status'='approved'
	        AND event NOT IN ('submitted','initial_decision','before_existing_course_merge','existing_course_merged'))`,
		requestIDs, target.ID, assignmentIDs, periodIDs).Scan(&pending, &foreignAssignment, &workStarted, &signed, &changedAfterApproval); err != nil {
		return nil, nil, nil, err
	}
	if pending {
		blockers = append(blockers, "วิชาหลักมีคำขอ TA ที่ยังรอพิจารณา ต้องพิจารณาคำขอให้เสร็จก่อนรวม")
	}
	if foreignAssignment {
		blockers = append(blockers, "พบการมอบหมายที่อ้างอิงข้ามวิชา กรุณาตรวจสอบคำขอและ Section ก่อนรวม")
	}
	if workStarted {
		blockers = append(blockers, "วิชาหลักมีบันทึกเวลาปฏิบัติงานแล้ว รวมถึงรายการร่าง ต้องรวมวิชาก่อนเริ่มบันทึกเวลา")
	}
	if signed {
		blockers = append(blockers, "วิชาหลักมีการตรวจหรือส่งเวลาประจำเดือนแล้ว ไม่สามารถรวมวิชาได้")
	}
	if changedAfterApproval {
		blockers = append(blockers, "วิชาหลักมีการแก้ภาระงานหรือเพิ่ม Section หลังอนุมัติคำขอแล้ว กรุณาตรวจสอบก่อนรวม")
	}

	// Only approval-stage records that remain attached to unchanged target IDs
	// are exempted. Unknown future references remain blockers at every level.
	checks := []struct {
		parent  string
		ids     []uuid.UUID
		allowed map[string]bool
	}{
		{"teaching_courses", []uuid.UUID{target.ID}, map[string]bool{"sections": true, "teaching_lecturers": true, "ta_requests": true, "course_budget_alerts": true, "submission_period_status": true, "tdbm_extra_teachings": true, "ta_request_drafts": true}},
		{"sections", sectionIDs, map[string]bool{"section_schedules": true, "exam_schedules": true, "ta_request_counts": true, "ta_request_assignments": true, "tdbm_extra_teachings": true}},
		{"ta_requests", requestIDs, map[string]bool{"ta_request_counts": true, "ta_request_assignments": true, "ta_request_calculation_snapshots": true}},
		{"ta_request_assignments", assignmentIDs, map[string]bool{"ta_workload_forms": true}},
		{"ta_workload_forms", workloadIDs, nil},
		{"submission_period_status", periodIDs, nil},
	}
	for _, check := range checks {
		used, e := existingMergeHasUsage(ctx, tx, check.parent, check.ids, check.allowed)
		if e != nil {
			return nil, nil, nil, e
		}
		if used {
			if !workStarted {
				blockers = append(blockers, "วิชาหลักมีข้อมูลใช้งานต่อจากขั้นอนุมัติ เช่น ตารางปฏิบัติงาน เอกสารส่งออก หรือรายการจ่ายแล้ว ไม่สามารถรวมวิชาได้")
			}
			break
		}
	}

	approvedIDs := []uuid.UUID{}
	rows, err := tx.Query(ctx, `SELECT id FROM ta_requests WHERE id=ANY($1::uuid[]) AND status='approved' ORDER BY id`, requestIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		approvedIDs = append(approvedIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, nil, err
	}
	var evidence json.RawMessage
	err = tx.QueryRow(ctx, `SELECT jsonb_build_object(
	 'requests',COALESCE((SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id) FROM ta_requests r WHERE r.id=ANY($1::uuid[])),'[]'::jsonb),
	 'counts',COALESCE((SELECT jsonb_agg(to_jsonb(c) ORDER BY c.request_id,c.section_id) FROM ta_request_counts c WHERE c.request_id=ANY($1::uuid[])),'[]'::jsonb),
	 'assignments',COALESCE((SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id) FROM ta_request_assignments a WHERE a.id=ANY($2::uuid[])),'[]'::jsonb),
	 'workloads',COALESCE((SELECT jsonb_agg(to_jsonb(w) ORDER BY w.id) FROM ta_workload_forms w WHERE w.id=ANY($3::uuid[])),'[]'::jsonb),
	 'periods',COALESCE((SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM submission_period_status p WHERE p.id=ANY($4::uuid[])),'[]'::jsonb)
	)`, requestIDs, assignmentIDs, workloadIDs, periodIDs).Scan(&evidence)
	return evidence, approvedIDs, blockers, err
}
