package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ta-payment-back/internal/audit"
)

// AddSectionsInput adds sections to a TA who is already on a request.
type AddSectionsInput struct {
	TAID     uuid.UUID         `json:"ta_id"`
	Sections []SectionWorkload `json:"sections"`
}

// AddSections puts more sections of the same course on a TA who is already on
// the request, without cancelling it.
//
// Cancel-and-resubmit was the only way before, and it is refused once the TA
// has logged hours — the very moment a lecturer is most likely to want to hand
// over another group. The new sections are appended to the EXISTING request,
// not filed as a second one, because every per-request rule reads the TA's
// sections together: the daily-hour feasibility, the graduate 10–12 total and
// the co-taught "billed once" grouping. A second request would let a TA pass
// each of them twice.
//
// Nothing already on the request changes except the co-taught grouping, which
// is recomputed over the combined set (a new section taught with an old one is
// the same sitting). The new rows are judged with the same clash rule a fresh
// request gets; a cross-course clash refuses the whole call with no rows written.
func (s *TARequestService) AddSections(ctx context.Context, lecturerID, reqID uuid.UUID, in AddSectionsInput) (*CreateResult, error) {
	return s.addSections(ctx, lecturerID, false, reqID, in)
}

// AddSectionsOnBehalf is AddSections run by an officer for the request's own
// lecturer (see CreateOnBehalf). No teaching check — staff act on any course —
// and the lecturer the request belongs to is told afterwards.
func (s *TARequestService) AddSectionsOnBehalf(ctx context.Context, actor, reqID uuid.UUID, in AddSectionsInput) (*CreateResult, error) {
	res, err := s.addSections(ctx, actor, true, reqID, in)
	if err != nil {
		return nil, err
	}
	var lecturerID, courseID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT lecturer_id, teaching_course_id FROM ta_requests WHERE id = $1`, reqID).Scan(&lecturerID, &courseID); err == nil {
		s.notifyFiledOnBehalf(ctx, actor, lecturerID, courseID, "เพิ่ม section ในคำขอผู้ช่วยสอน")
	}
	return res, nil
}

func (s *TARequestService) addSections(ctx context.Context, actor uuid.UUID, privileged bool, reqID uuid.UUID, in AddSectionsInput) (*CreateResult, error) {
	if in.TAID == uuid.Nil || len(in.Sections) == 0 {
		return nil, errors.New("ต้องเลือก section ที่จะเพิ่มอย่างน้อย 1 กลุ่ม")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var courseID uuid.UUID
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT teaching_course_id, status::text FROM ta_requests WHERE id = $1 FOR UPDATE`, reqID,
	).Scan(&courseID, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errors.New("ไม่พบคำขอนี้")
		}
		return nil, err
	}
	teaches := privileged
	if !teaches {
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM teaching_lecturers
			  WHERE teaching_course_id = $1 AND lecturer_id = $2)`, courseID, actor).Scan(&teaches); err != nil {
			return nil, err
		}
	}
	if !teaches {
		return nil, errors.New("คุณไม่ได้เป็นผู้สอนของรายวิชานี้")
	}
	if status != "submitted" && status != "approved" {
		label := statusLabelTH[status]
		if label == "" {
			label = status
		}
		return nil, fmt.Errorf("เพิ่ม section ในคำขอนี้ไม่ได้ (สถานะปัจจุบัน: %s)", label)
	}
	var termID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT term_id FROM teaching_courses WHERE id = $1`, courseID).Scan(&termID); err != nil {
		return nil, err
	}

	// The TA must already be on this request; their level and enrollment
	// snapshot carry over from the existing rows so one request never holds
	// two versions of the same person.
	name := s.taName(ctx, in.TAID)
	var level string
	var enrollmentID *uuid.UUID
	var studentID *string
	if err := tx.QueryRow(ctx, `
		SELECT level::text, enrollment_id, student_id_snapshot
		FROM ta_request_assignments
		WHERE request_id = $1 AND ta_id = $2 AND state <> 'dropped'
		ORDER BY state_decided_at NULLS FIRST LIMIT 1`, reqID, in.TAID,
	).Scan(&level, &enrollmentID, &studentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%s ไม่ได้อยู่ในคำขอนี้", name)
		}
		return nil, err
	}
	isGrad := level == "master" || level == "phd"
	if isGrad {
		var frozen bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM grad_lump_ledger WHERE teaching_course_id = $1 AND ta_id = $2)`,
			courseID, in.TAID).Scan(&frozen); err != nil {
			return nil, err
		}
		if frozen {
			return nil, Conflict("เพิ่ม section ไม่ได้ เพราะยอดเหมาจ่ายของ " + name + " ในรายวิชานี้ถูกบันทึกแล้ว กรุณาติดต่อเจ้าหน้าที่")
		}
	}

	// Sections this TA already holds on the course, in any live request.
	held := map[uuid.UUID]bool{}
	var existing []uuid.UUID
	rows, err := tx.Query(ctx, `
		SELECT a.section_id, a.request_id = $3
		FROM ta_request_assignments a
		JOIN ta_requests r ON r.id = a.request_id
		WHERE a.ta_id = $1 AND r.teaching_course_id = $2
		  AND r.status IN ('submitted', 'approved') AND a.state <> 'dropped'`, in.TAID, courseID, reqID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sid uuid.UUID
		var sameReq bool
		if err := rows.Scan(&sid, &sameReq); err != nil {
			rows.Close()
			return nil, err
		}
		held[sid] = true
		if sameReq {
			existing = append(existing, sid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var added []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, sw := range in.Sections {
		sid := sw.SectionID
		if seen[sid] {
			return nil, errors.New("มี section ซ้ำกันในรายการที่จะเพิ่ม")
		}
		seen[sid] = true
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM sections WHERE id = $1 AND teaching_course_id = $2)`,
			sid, courseID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("พบ section ที่ไม่ได้อยู่ในรายวิชานี้")
		}
		secNo := s.sectionLabel(ctx, sid)
		if held[sid] {
			return nil, fmt.Errorf("%s ดูแล Sec %s ของวิชานี้อยู่แล้ว", name, secNo)
		}
		// The unique key is (request, section, TA): a row that was dropped for
		// a clash still occupies it, and re-adding would reach the same verdict.
		var dropped bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM ta_request_assignments
			  WHERE request_id = $1 AND section_id = $2 AND ta_id = $3)`, reqID, sid, in.TAID).Scan(&dropped); err != nil {
			return nil, err
		}
		if dropped {
			return nil, fmt.Errorf("Sec %s เคยถูกตัดออกจากคำขอนี้เพราะตรงกับตารางเรียนของ %s ทุกคาบ จึงเพิ่มซ้ำไม่ได้", secNo, name)
		}
		// A clash with another course the TA assists blocks a fresh request;
		// it blocks this too.
		if err := s.checkCrossRequestConflict(ctx, tx, in.TAID, sid, termID, courseID, reqID, []string{"approved"}, name); err != nil {
			return nil, err
		}
		added = append(added, sid)
	}

	// Validate the new sections' figures exactly as Create does, then the
	// combined per-TA totals over old + new.
	per := resolveSectionWorkloads(added, WorkloadInput{}, in.Sections)
	weekly, err := s.sectionWeeklyHours(ctx, added)
	if err != nil {
		return nil, err
	}
	for _, sid := range added {
		w := per[sid]
		secNo := s.sectionLabel(ctx, sid)
		if err := validateWorkloadFields(w, name); err != nil {
			return nil, err
		}
		if isGrad {
			if err := validateGradWorkloadCaps(w, name, secNo); err != nil {
				return nil, err
			}
		} else if err := validateUndergradSectionCaps(w, name, secNo, weekly[sid]); err != nil {
			return nil, err
		}
	}
	all := append(append([]uuid.UUID{}, existing...), added...)
	for _, sid := range existing {
		w, err := scanWorkloadForm(tx.QueryRow(ctx, `
			SELECT `+workloadFormColumns+` FROM ta_workload_forms wf
			JOIN ta_request_assignments a ON a.id = wf.assignment_id
			WHERE a.request_id = $1 AND a.ta_id = $2 AND a.section_id = $3`, reqID, in.TAID, sid))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		per[sid] = w
	}
	groups, err := s.detectCotaughtGroups(ctx, all)
	if err != nil {
		return nil, err
	}
	total := billableWeeklyTotal(all, per, groups, level)
	if isGrad {
		if total > 12+0.001 {
			return nil, fmt.Errorf("รวมแล้ว %s มีภาระงาน %.2f ชม./สัปดาห์ เกิน 12 ชม. ตามระเบียบบัณฑิตศึกษา", name, total)
		}
	} else if err := s.enforceDailyHourFeasibility(ctx, all, level, name, total); err != nil {
		return nil, err
	}

	for _, sid := range added {
		aid := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO ta_request_assignments (id, request_id, section_id, ta_id, level, enrollment_id, student_id_snapshot)
			 VALUES ($1,$2,$3,$4,$5::study_level,$6,$7)`,
			aid, reqID, sid, in.TAID, level, enrollmentID, studentID); err != nil {
			return nil, err
		}
		wl := per[sid]
		if _, err := tx.Exec(ctx, `
			INSERT INTO ta_workload_forms (id, assignment_id, help_teach_hrs, help_teach_desc, prep_hrs, prep_desc,
				grade_hrs, grade_desc, other_hrs, other_desc, check_work_hrs, attendance_hrs, ug_other_hrs, ug_other_desc,
				lab_hrs, lab_other_hrs, lab_other_desc)
			VALUES (gen_random_uuid(), $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			aid, wl.HelpTeachHrs, wl.HelpTeachDesc, wl.PrepHrs, wl.PrepDesc, wl.GradeHrs, wl.GradeDesc,
			wl.OtherHrs, wl.OtherDesc, wl.CheckWorkHrs, wl.AttendanceHrs, wl.UGOtherHrs, wl.UGOtherDesc,
			wl.LabHrs, wl.LabOtherHrs, wl.LabOtherDesc); err != nil {
			return nil, err
		}
	}
	// Co-taught grouping over the combined set; NULL for singletons, as Create.
	for _, sid := range all {
		var g any
		if countInGroup(groups, groups[sid]) > 1 {
			g = groups[sid]
		}
		if _, err := tx.Exec(ctx, `
			UPDATE ta_request_assignments SET cotaught_group = $1
			WHERE request_id = $2 AND ta_id = $3 AND section_id = $4`, g, reqID, in.TAID, sid); err != nil {
			return nil, err
		}
	}

	// Judge the new rows now if the TA has a timetable; otherwise the request
	// is still waiting on it and ReevaluateForTA decides everything together.
	var notices map[uuid.UUID][]string
	missing, err := tasMissingSchedule(ctx, tx, reqID, termID)
	if err != nil {
		return nil, err
	}
	if status == "approved" && len(missing) == 0 {
		notices, err = s.applyClashOutcomeFor(ctx, tx, reqID, in.TAID, true)
		if err != nil {
			return nil, err
		}
	}

	labels := make([]string, 0, len(added))
	for _, sid := range added {
		labels = append(labels, s.sectionLabel(ctx, sid))
	}
	if err := s.aud.LogTx(ctx, tx, audit.Entry{
		ActorID: &actor, Action: "ta_request.add_sections",
		Entity: "ta_request", EntityID: reqID.String(),
		Note:  fmt.Sprintf("ta=%s sec=%s", in.TAID, strings.Join(labels, ",")),
		After: in,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	var checks []DecisionCheck
	for taID, lines := range notices {
		checks = append(checks, DecisionCheck{
			Rule: "clash_trimmed", TAName: s.taName(ctx, taID),
			Passed: false, Warning: true, Message: strings.Join(lines, "\n"),
		})
	}
	s.notifyClashOutcome(ctx, reqID, notices)
	s.notifySectionsAdded(ctx, reqID, in.TAID, labels)
	return &CreateResult{ID: reqID, Status: status, Checks: checks}, nil
}

// notifySectionsAdded tells the TA they now cover more groups. The course and
// section numbers go in the title: unread notices with the same title and link
// are folded into one.
func (s *TARequestService) notifySectionsAdded(ctx context.Context, reqID, taID uuid.UUID, secLabels []string) {
	if s.notify == nil || len(secLabels) == 0 {
		return
	}
	var courseID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT teaching_course_id FROM ta_requests WHERE id = $1`, reqID).Scan(&courseID); err != nil {
		return
	}
	code, nameTH := s.courseLabel(ctx, courseID)
	secs := strings.Join(secLabels, ", ")
	s.notify.Send(ctx, taID,
		fmt.Sprintf("ได้รับมอบหมายเพิ่ม %s Sec %s", code, secs),
		fmt.Sprintf("อาจารย์ผู้สอนรายวิชา %s %s ได้มอบหมายให้ท่านดูแลกลุ่มเรียน Sec %s เพิ่มเติม", code, nameTH, secs),
		"/ta")
}
