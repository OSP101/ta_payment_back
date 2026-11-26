package service

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TDBM staging matches and a retained main-course request draft are not TA
// work. Lock and snapshot them so inspection still rejects concurrent changes.
func existingMergeImportReferences(ctx context.Context, tx pgx.Tx, target, source ExistingMergeCourse) (json.RawMessage, error) {
	ids := []uuid.UUID{target.ID, source.ID}
	sections := []uuid.UUID{}
	for _, c := range []ExistingMergeCourse{target, source} {
		for _, sec := range c.Sections {
			sections = append(sections, sec.ID)
		}
	}
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{`SELECT extra_class_id FROM tdbm_extra_teachings WHERE teaching_course_id=ANY($1::uuid[]) OR section_id=ANY($2::uuid[]) ORDER BY extra_class_id FOR UPDATE`, []any{ids, sections}},
		{`SELECT user_id FROM ta_request_drafts WHERE teaching_course_id=$1 ORDER BY user_id FOR UPDATE`, []any{target.ID}},
	} {
		rows, err := tx.Query(ctx, query.sql, query.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	var raw json.RawMessage
	err := tx.QueryRow(ctx, `SELECT jsonb_build_object(
  'tdbm',COALESCE((SELECT jsonb_agg(to_jsonb(t) ORDER BY extra_class_id) FROM tdbm_extra_teachings t WHERE teaching_course_id=ANY($1::uuid[]) OR section_id=ANY($2::uuid[])),'[]'::jsonb),
  'target_request_drafts',COALESCE((SELECT jsonb_agg(to_jsonb(d) ORDER BY user_id) FROM ta_request_drafts d WHERE teaching_course_id=$3),'[]'::jsonb)
 )`, ids, sections, target.ID).Scan(&raw)
	return raw, err
}

// The registrar section number of an absorbed code can differ from the chosen
// destination (SC section 2 -> CP section 3). Keep using the reviewed mapping
// on later TDBM syncs rather than matching it to CP's unrelated section 2.
const existingMergeTDBMSectionMatch = `
 SELECT dst.id
 FROM teaching_courses tc
 CROSS JOIN LATERAL jsonb_array_elements(tc.reg_source) evidence
 CROSS JOIN LATERAL jsonb_array_elements(evidence->'existing_course_merge'->'source'->'sections') src
 CROSS JOIN LATERAL jsonb_array_elements(evidence->'section_mappings') mapping
 JOIN sections dst ON dst.id::text=COALESCE(mapping->>'target',src->>'id')
 WHERE tc.id=t2.teaching_course_id AND dst.teaching_course_id=tc.id
   AND (evidence->'existing_course_merge'->'source'->>'code'=t2.course_code
        OR COALESCE(evidence->'existing_course_merge'->'source'->'alt_codes','[]'::jsonb) ? t2.course_code)
   AND mapping->>'source'=src->>'id'
   AND ltrim(src->>'sec_no','0')=ltrim(regexp_replace(COALESCE(t2.section_label,''),'\D','','g'),'0')
 ORDER BY (src->>'track'=CASE t2.semester_type WHEN 'ภาคพิเศษ' THEN 'special' ELSE 'regular' END) DESC
 LIMIT 1`
