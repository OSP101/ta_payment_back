package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"ta-payment-back/internal/audit"
)

// CommitREGReview is all-or-nothing. File identity, corrections, active users,
// warning acknowledgements and the database snapshot are checked before any
// course is written, then checked/locked again inside the transaction.
func (s *TeachingService) CommitREGReview(ctx context.Context, actor, term uuid.UUID, filename string, body []byte, input *REGReview) (*ImportResult, error) {
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return nil, err
	}
	if !priv {
		return nil, ErrForbidden
	}
	if input == nil || !input.Confirmed {
		return nil, Invalid("กรุณาตรวจสอบและยืนยันข้อมูลก่อนบันทึก")
	}
	expected := input.ValidationHash
	r, source, err := s.prepareREGReview(ctx, term, body, input)
	if err != nil {
		return nil, err
	}
	if !r.Ready {
		return nil, Invalid("ยังมีข้อมูลที่ต้องแก้ไขหรือรายการที่ยังไม่ได้ยืนยัน กรุณาตรวจสอบอีกครั้ง")
	}
	if expected == "" || expected != r.ValidationHash {
		return nil, Conflict("ข้อมูลเปลี่ยนหลังตรวจสอบ กรุณากดตรวจสอบอีกครั้งก่อนบันทึก")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "reg-import:"+term.String()); err != nil {
		return nil, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM academic_terms WHERE id=$1)`, term).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, Invalid("ไม่พบภาคการศึกษา")
	}
	for _, c := range r.Courses {
		current, version, e := regExistingSnapshot(ctx, tx, term, c.Code)
		if e != nil {
			err = e
			return nil, err
		}
		if current != c.ExistingID || version != c.ExistingVersion {
			return nil, Conflict("รายวิชาในระบบเปลี่ยน กรุณาตรวจสอบไฟล์อีกครั้ง")
		}
		if !c.Skip {
			if err = assertActiveLecturers(ctx, tx, c.LecturerIDs); err != nil {
				return nil, err
			}
		}
	}
	res := &ImportResult{RowCount: len(r.Courses), CreatedCodes: []string{}, SkippedCodes: []string{}, MergedCodes: []string{}, Warnings: []string{}, Errors: []string{}}
	byCode := map[string]*parsedCourse{}
	for _, c := range source {
		byCode[c.code] = c
	}
	ids := map[string]uuid.UUID{}
	affected := map[uuid.UUID]bool{}
	for _, c := range r.Courses {
		if c.ExistingID != "" {
			ids[c.Code] = uuid.MustParse(c.ExistingID)
		}
		if c.Skip {
			res.SkippedCodes = append(res.SkippedCodes, c.Code)
			continue
		}
		if c.MergeInto != "" {
			continue
		}
		id := uuid.New()
		cr, l, p, ss, _ := parseUnit(c.Unit)
		if _, err = tx.Exec(ctx, `INSERT INTO teaching_courses(id,term_id,code,name_th,name_en,level,credits,lecture_hrs,lab_hrs,self_hrs,created_by,reg_source) VALUES($1,$2,$3,$4,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`, id, term, c.Code, c.Name, c.Level, cr, l, p, ss, actor, regSourceData(byCode[c.Code], c)); err != nil {
			return nil, err
		}
		for i, lid := range c.LecturerIDs {
			if _, err = tx.Exec(ctx, `INSERT INTO teaching_lecturers(teaching_course_id,lecturer_id,is_primary) VALUES($1,$2,$3)`, id, lid, i == 0); err != nil {
				return nil, err
			}
		}
		for _, sec := range c.Sections {
			pending := len(sec.Schedules) == 0
			for _, sch := range sec.Schedules {
				if sch.Pending {
					pending = true
				}
			}
			sid := uuid.New()
			var room *string
			for _, sch := range sec.Schedules {
				if !sch.Pending && sch.Room != "" {
					room = emptyToNil(sch.Room)
					break
				}
			}
			if _, err = tx.Exec(ctx, `INSERT INTO sections(id,teaching_course_id,sec_no,track,room,num_students,curriculum,reg_reserved_for,reg_pending_schedule,reg_pending_reason) VALUES($1,$2,$3,$4::section_track,$5,$6,$7,$8,$9,$10)`, sid, id, sec.SecNo, sec.Track, room, *sec.Students, sec.Curriculum, sec.ReservedFor, pending, sec.PendingReason); err != nil {
				return nil, err
			}
			for _, sch := range sec.Schedules {
				if sch.Pending {
					continue
				}
				if _, err = tx.Exec(ctx, `INSERT INTO section_schedules(id,section_id,kind,day_of_week,start_time,end_time,room) VALUES($1,$2,$3,$4,$5::time,$6::time,$7)`, uuid.New(), sid, sch.Kind, sch.Day, sch.Start, sch.End, emptyToNil(sch.Room)); err != nil {
					return nil, err
				}
			}
		}
		if err = s.recomputeAggregate(ctx, tx, id); err != nil {
			return nil, err
		}
		ids[c.Code] = id
		affected[id] = true
		res.CreatedIDs = append(res.CreatedIDs, id)
		res.CreatedCodes = append(res.CreatedCodes, c.Code)
	}
	for _, c := range r.Courses {
		if c.Skip || c.MergeInto == "" {
			continue
		}
		target, ok := ids[c.MergeInto]
		if !ok {
			return nil, Invalid("ไม่พบวิชาหลัก " + c.MergeInto)
		}
		if err = s.assertNotExported(ctx, tx, target); err != nil {
			return nil, err
		}
		secs := []mergeSection{}
		for _, sec := range c.Sections {
			pending := len(sec.Schedules) == 0
			for _, sch := range sec.Schedules {
				if sch.Pending {
					pending = true
				}
			}
			v := mergeSection{regReserved: sec.ReservedFor, regPending: pending, regPendingReason: sec.PendingReason, secNo: sec.SecNo, track: sec.Track, numStudents: *sec.Students, curriculum: emptyToNil(sec.Curriculum)}
			for _, sch := range sec.Schedules {
				if !sch.Pending {
					v.schedules = append(v.schedules, parsedSchedule{kind: sch.Kind, dow: sch.Day, startTime: sch.Start, endTime: sch.End, room: sch.Room})
				}
			}
			secs = append(secs, v)
		}
		if err = s.mergeCodeTx(ctx, tx, term, target, c.Code, c.LecturerIDs, secs); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `UPDATE teaching_courses SET reg_source=reg_source || $2::jsonb WHERE id=$1`, target, regSourceData(byCode[c.Code], c)); err != nil {
			return nil, err
		}
		affected[target] = true
		res.MergedCodes = append(res.MergedCodes, c.Code+" → "+c.MergeInto)
	}
	for id := range affected {
		if err := assertCourseTotalStudents(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	for _, i := range r.Issues {
		if i.Severity == "warning" {
			res.Warnings = append(res.Warnings, i.Code+": "+i.Message)
		}
	}
	res.WarningCount = len(res.Warnings)
	// Store both source cells and reviewed values, including skipped courses.
	evidence := map[string]json.RawMessage{}
	for _, c := range r.Courses {
		evidence[c.Code] = regSourceData(byCode[c.Code], c)
	}
	data, err := json.Marshal(map[string]any{"review": r, "sources": evidence})
	if err != nil {
		return nil, err
	}
	summary := map[string]any{"created_count": len(res.CreatedIDs), "merged_count": len(res.MergedCodes), "skipped_count": len(res.SkippedCodes), "warning_count": res.WarningCount, "validation_hash": r.ValidationHash}
	if _, err = tx.Exec(ctx, `INSERT INTO schedule_imports(id,imported_by,filename,row_count,error_count,summary,at,term_id,file_sha256,started_at,created_count,skipped_count,merged_count,warning_count,created_codes,skipped_codes,merged_codes,warnings,errors,review_data) VALUES($1,$2,$3,$4,0,$5,NOW(),$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18::jsonb)`, uuid.New(), actor, filename, res.RowCount, summary, term, r.FileSHA256, time.Now(), len(res.CreatedIDs), len(res.SkippedCodes), len(res.MergedCodes), res.WarningCount, notNilStrs(res.CreatedCodes), notNilStrs(res.SkippedCodes), notNilStrs(res.MergedCodes), notNilStrs(res.Warnings), []string{}, data); err != nil {
		return nil, err
	}
	if s.aud != nil {
		if err = s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "schedule.import", Entity: "term", EntityID: term.String(), After: summary, Note: fmt.Sprintf("REG reviewed %s", r.FileSHA256)}); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	if s.tdbm != nil {
		s.tdbm.ResolveMatchesForTermID(ctx, term)
	}
	return res, nil
}
