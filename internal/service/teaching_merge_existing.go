package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"ta-payment-back/internal/audit"
)

type ExistingMergeMapping struct {
	Source uuid.UUID  `json:"source"`
	Target *uuid.UUID `json:"target"`
}
type ExistingMergeInput struct {
	SourceID  uuid.UUID              `json:"source_id"`
	Mappings  []ExistingMergeMapping `json:"mappings"`
	Token     string                 `json:"token"`
	Confirmed bool                   `json:"confirmed"`
}
type ExistingMergeSection struct {
	ID       uuid.UUID       `json:"id"`
	SecNo    string          `json:"sec_no"`
	Track    string          `json:"track"`
	Students int             `json:"students"`
	Data     json.RawMessage `json:"data"`
}
type ExistingMergeCourse struct {
	ID       uuid.UUID              `json:"id"`
	Term     uuid.UUID              `json:"term_id"`
	Code     string                 `json:"code"`
	Name     string                 `json:"name"`
	AltCodes []string               `json:"alt_codes"`
	Credits  int                    `json:"credits"`
	Lecture  int                    `json:"lecture"`
	Lab      int                    `json:"lab"`
	Self     int                    `json:"self"`
	Level    string                 `json:"level"`
	Exported bool                   `json:"exported"`
	Data     json.RawMessage        `json:"data"`
	Sections []ExistingMergeSection `json:"sections"`
}
type ExistingMergePreview struct {
	Source   ExistingMergeCourse `json:"source"`
	Target   ExistingMergeCourse `json:"target"`
	Blockers []string            `json:"blockers"`
	Token    string              `json:"token"`
}

func existingMergeCourse(ctx context.Context, tx pgx.Tx, id uuid.UUID) (c ExistingMergeCourse, err error) {
	err = tx.QueryRow(ctx, `SELECT id,term_id,code,name_th,alt_codes,credits,lecture_hrs,lab_hrs,self_hrs,level::text,exported_at IS NOT NULL,to_jsonb(c) FROM teaching_courses c WHERE id=$1 FOR UPDATE`, id).Scan(&c.ID, &c.Term, &c.Code, &c.Name, &c.AltCodes, &c.Credits, &c.Lecture, &c.Lab, &c.Self, &c.Level, &c.Exported, &c.Data)
	if err != nil {
		return c, err
	}
	rows, err := tx.Query(ctx, `SELECT id,sec_no,track::text,num_students FROM sections WHERE teaching_course_id=$1 ORDER BY id FOR UPDATE`, id)
	if err != nil {
		return c, err
	}
	c.Sections = []ExistingMergeSection{}
	for rows.Next() {
		var s ExistingMergeSection
		if err = rows.Scan(&s.ID, &s.SecNo, &s.Track, &s.Students); err != nil {
			rows.Close()
			return c, err
		}
		c.Sections = append(c.Sections, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return c, err
	}
	for i := range c.Sections {
		s := &c.Sections[i]
		err = tx.QueryRow(ctx, `SELECT to_jsonb(s)||jsonb_build_object('schedules',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.id) FROM section_schedules x WHERE x.section_id=s.id),'[]'::jsonb),'exams',COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.id) FROM exam_schedules x WHERE x.section_id=s.id),'[]'::jsonb)) FROM sections s WHERE s.id=$1`, s.ID).Scan(&s.Data)
		if err != nil {
			return c, err
		}
	}
	var lecturers json.RawMessage
	err = tx.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(l)||jsonb_build_object('name',u.first_name||' '||u.last_name,'email',u.email) ORDER BY l.lecturer_id),'[]'::jsonb) FROM teaching_lecturers l JOIN users u ON u.id=l.lecturer_id WHERE l.teaching_course_id=$1`, id).Scan(&lecturers)
	if err == nil {
		var v map[string]json.RawMessage
		err = json.Unmarshal(c.Data, &v)
		if err == nil {
			v["lecturers"] = lecturers
			c.Data, err = json.Marshal(v)
		}
	}
	return c, err
}

// Reject every operational reference, including future FK-backed features.
// Only the basic roster/timetable tables are transferable here. Row locks on
// both parents also serialize concurrent FK inserts against the final checks.
func existingMergeHasUsage(ctx context.Context, tx pgx.Tx, parent string, ids []uuid.UUID, allowed map[string]bool) (bool, error) {
	if len(ids) == 0 {
		return false, nil
	}
	rows, err := tx.Query(ctx, `SELECT n.nspname,c.relname,a.attname FROM pg_constraint fk JOIN pg_class c ON c.oid=fk.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace JOIN LATERAL unnest(fk.conkey,fk.confkey) k(child,parent) ON true JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=k.child JOIN pg_attribute p ON p.attrelid=fk.confrelid AND p.attnum=k.parent WHERE fk.contype='f' AND fk.confrelid=$1::regclass AND p.attname='id' ORDER BY n.nspname,c.relname,a.attname`, parent)
	if err != nil {
		return false, err
	}
	refs := [][3]string{}
	for rows.Next() {
		var r [3]string
		if err = rows.Scan(&r[0], &r[1], &r[2]); err != nil {
			rows.Close()
			return false, err
		}
		refs = append(refs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, r := range refs {
		if r[0] == "public" && allowed[r[1]] {
			continue
		}
		var found bool
		q := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE %s=ANY($1))`, pgx.Identifier{r[0], r[1]}.Sanitize(), pgx.Identifier{r[2]}.Sanitize())
		if err = tx.QueryRow(ctx, q, ids).Scan(&found); err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}
func prepareExistingMerge(ctx context.Context, tx pgx.Tx, target uuid.UUID, in ExistingMergeInput) (p ExistingMergePreview, err error) {
	p.Blockers = []string{}
	if target == in.SourceID || in.SourceID == uuid.Nil {
		return p, Invalid("เลือกสองรายวิชาที่แตกต่างกัน")
	}
	ids := []uuid.UUID{target, in.SourceID}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		c, e := existingMergeCourse(ctx, tx, id)
		if e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				return p, Conflict("รายวิชาเปลี่ยนหรือถูกรวมแล้ว กรุณาโหลดรายการวิชาใหม่")
			}
			return p, e
		}
		if id == target {
			p.Target = c
		} else {
			p.Source = c
		}
	}
	if p.Target.Term != p.Source.Term {
		p.Blockers = append(p.Blockers, "รวมได้เฉพาะวิชาในภาคการศึกษาเดียวกัน")
	}
	if p.Target.Exported || p.Source.Exported {
		p.Blockers = append(p.Blockers, "วิชาถูกล็อกหลังส่งออกเอกสารแล้ว ต้องจัดการเอกสารและปลดล็อกก่อน")
	}
	if p.Target.Credits != p.Source.Credits || p.Target.Lecture != p.Source.Lecture || p.Target.Lab != p.Source.Lab || p.Target.Self != p.Source.Self || p.Target.Level != p.Source.Level {
		p.Blockers = append(p.Blockers, "หน่วยกิต ชั่วโมง หรือระดับการศึกษาต่างกัน ต้องตรวจแก้ข้อมูลให้ตรงก่อนรวม")
	}
	if primaryCode(p.Target.Code, append([]string{p.Source.Code}, p.Source.AltCodes...)) != p.Target.Code {
		p.Blockers = append(p.Blockers, "เลือกรหัสหลักสูตรใหม่กว่าเป็นวิชาหลัก (CP ก่อน SC ก่อนรหัสตัวเลข)")
	}
	sectionIDs := []uuid.UUID{}
	for _, c := range []ExistingMergeCourse{p.Target, p.Source} {
		for _, s := range c.Sections {
			sectionIDs = append(sectionIDs, s.ID)
		}
	}
	used, e := existingMergeHasUsage(ctx, tx, "teaching_courses", ids, map[string]bool{"sections": true, "teaching_lecturers": true})
	if e != nil {
		return p, e
	}
	sectionUsed, e := existingMergeHasUsage(ctx, tx, "sections", sectionIDs, map[string]bool{"section_schedules": true, "exam_schedules": true})
	if e != nil {
		return p, e
	}
	if used || sectionUsed {
		p.Blockers = append(p.Blockers, "มีข้อมูลการใช้งาน เช่น คำขอ TA การมอบหมาย งานเบิกจ่าย หรือข้อมูลเชื่อมโยงอยู่แล้ว กรุณาให้ผู้ดูแลตรวจรายการที่ผูกอยู่ก่อนรวม")
	}
	targets := map[uuid.UUID]ExistingMergeSection{}
	totals := map[uuid.UUID]int{}
	for _, s := range p.Target.Sections {
		targets[s.ID] = s
		totals[s.ID] = s.Students
	}
	sources := map[uuid.UUID]bool{}
	for _, s := range p.Source.Sections {
		sources[s.ID] = true
	}
	seen := map[uuid.UUID]bool{}
	for _, m := range in.Mappings {
		if !sources[m.Source] || seen[m.Source] {
			return p, Invalid("รายการจับคู่ Section ไม่ถูกต้องหรือซ้ำ")
		}
		seen[m.Source] = true
		if m.Target != nil {
			dst, ok := targets[*m.Target]
			if !ok {
				return p, Invalid("Section ปลายทางต้องเป็นของวิชาหลัก")
			}
			for _, src := range p.Source.Sections {
				if src.ID == m.Source && src.Track != dst.Track {
					p.Blockers = append(p.Blockers, "Section ที่รวมจำนวนผู้เรียนต้องเป็นประเภทภาคเดียวกัน")
				}
			}
			for _, src := range p.Source.Sections {
				if src.ID == m.Source {
					totals[dst.ID] += src.Students
				}
			}
		}
	}
	if len(seen) != len(sources) {
		p.Blockers = append(p.Blockers, "กำหนดวิธีจัดการให้ครบทุก Section ของวิชาที่จะรวม")
	}
	total := 0
	for _, s := range p.Target.Sections {
		total += s.Students
	}
	for _, s := range p.Source.Sections {
		total += s.Students
	}
	if total > 100000 {
		p.Blockers = append(p.Blockers, "จำนวนผู้เรียนรวมเกินขอบเขตที่ระบบรองรับ")
	}
	for _, n := range totals {
		if e := validateSectionStudents(n, "Section หลังรวม"); e != nil {
			p.Blockers = append(p.Blockers, e.Error())
		}
	}
	snapshot, _ := json.Marshal(struct {
		Preview  ExistingMergePreview
		Mappings []ExistingMergeMapping
	}{p, in.Mappings})
	p.Token = regFileHash(snapshot)
	return p, nil
}
func (s *TeachingService) ExistingCourseMerge(ctx context.Context, actor, target uuid.UUID, in ExistingMergeInput, commit bool) (ExistingMergePreview, error) {
	var p ExistingMergePreview
	priv, err := isPrivileged(ctx, s.pool, actor)
	if err != nil {
		return p, err
	}
	if !priv {
		return p, ErrForbidden
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return p, err
	}
	defer tx.Rollback(ctx)
	p, err = prepareExistingMerge(ctx, tx, target, in)
	if err != nil {
		return p, err
	}
	if !commit {
		return p, nil
	}
	if !in.Confirmed || in.Token == "" || in.Token != p.Token {
		return p, Conflict("ข้อมูลเปลี่ยนหรือยังไม่ได้ยืนยัน กรุณาตรวจตัวอย่างใหม่")
	}
	if len(p.Blockers) > 0 {
		return p, Invalid(strings.Join(p.Blockers, "; "))
	}
	// Keep the main lecturer, append other active teachers without duplicates.
	var teacherIDs []uuid.UUID
	rows, err := tx.Query(ctx, `SELECT lecturer_id FROM teaching_lecturers WHERE teaching_course_id=ANY($1) ORDER BY lecturer_id`, []uuid.UUID{target, in.SourceID})
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return p, err
		}
		teacherIDs = append(teacherIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
	}
	if err = assertActiveLecturers(ctx, tx, teacherIDs); err != nil {
		return p, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO teaching_lecturers(teaching_course_id,lecturer_id,is_primary) SELECT $1,lecturer_id,is_primary AND NOT EXISTS(SELECT 1 FROM teaching_lecturers WHERE teaching_course_id=$1 AND is_primary) FROM teaching_lecturers WHERE teaching_course_id=$2 ON CONFLICT DO NOTHING`, target, in.SourceID); err != nil {
		return p, err
	}
	if _, err = tx.Exec(ctx, `UPDATE teaching_lecturers SET is_primary=true WHERE teaching_course_id=$1 AND lecturer_id=(SELECT lecturer_id FROM teaching_lecturers WHERE teaching_course_id=$2 AND is_primary LIMIT 1) AND NOT EXISTS(SELECT 1 FROM teaching_lecturers WHERE teaching_course_id=$1 AND is_primary)`, target, in.SourceID); err != nil {
		return p, err
	}
	for _, m := range in.Mappings {
		if m.Target == nil {
			var secNo string
			for _, s := range p.Source.Sections {
				if s.ID == m.Source {
					secNo = s.SecNo
				}
			}
			if _, err = tx.Exec(ctx, `UPDATE sections SET teaching_course_id=$1,sec_no=CASE WHEN course_code IS NULL THEN $2 ELSE sec_no END,course_code=COALESCE(course_code,$3) WHERE id=$4`, target, altSecNo(p.Source.Code, secNo), p.Source.Code, m.Source); err != nil {
				return p, err
			}
		} else {
			if _, err = tx.Exec(ctx, `UPDATE sections dst SET num_students=dst.num_students+src.num_students,reg_reserved_for=concat_ws(E'\n',NULLIF(dst.reg_reserved_for,''),NULLIF(src.reg_reserved_for,'')) FROM sections src WHERE dst.id=$1 AND src.id=$2`, *m.Target, m.Source); err != nil {
				return p, err
			}
			if _, err = tx.Exec(ctx, `DELETE FROM sections WHERE id=$1`, m.Source); err != nil {
				return p, err
			}
		}
	}
	codes := append([]string{p.Source.Code}, p.Source.AltCodes...)
	evidence, _ := json.Marshal(p)
	entered := func(track string) bool {
		present, known := false, true
		for _, c := range []ExistingMergeCourse{p.Target, p.Source} {
			has := false
			for _, sec := range c.Sections {
				if sec.Track == track {
					has = true
				}
			}
			if has {
				present = true
				var fields map[string]json.RawMessage
				if e := json.Unmarshal(c.Data, &fields); e != nil {
					return false
				}
				var flag bool
				_ = json.Unmarshal(fields["num_students_"+track+"_entered"], &flag)
				known = known && flag
			}
		}
		return present && known
	}
	regularEntered, specialEntered := entered("regular"), entered("special")
	if _, err = tx.Exec(ctx, `UPDATE teaching_courses dst SET alt_codes=ARRAY(SELECT DISTINCT c FROM unnest(dst.alt_codes||$2::text[]) c WHERE c<>dst.code ORDER BY c),reg_source=dst.reg_source||src.reg_source||jsonb_build_array(jsonb_build_object('existing_course_merge',$4::jsonb)),num_students_regular_entered=$5,num_students_special_entered=$6,updated_at=now() FROM teaching_courses src WHERE dst.id=$1 AND src.id=$3`, target, codes, in.SourceID, evidence, regularEntered, specialEntered); err != nil {
		return p, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM teaching_courses WHERE id=$1`, in.SourceID); err != nil {
		return p, err
	}
	if err = s.recomputeAggregate(ctx, tx, target); err != nil {
		return p, err
	}
	if err = s.aud.LogTx(ctx, tx, audit.Entry{ActorID: &actor, Action: "teaching_course.merge_existing", Entity: "teaching_course", EntityID: target.String(), Before: p, After: in}); err != nil {
		return p, err
	}
	if err = tx.Commit(ctx); err != nil {
		return p, err
	}
	s.rematchTDBM(ctx, target)
	return p, nil
}
