-- Refuse a lossy rollback: resolve legitimate cross-track numbers explicitly.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM sections GROUP BY teaching_course_id, sec_no HAVING COUNT(*) > 1) THEN
  RAISE EXCEPTION 'Cannot rollback REG review: section numbers repeat across tracks';
 END IF;
END $$;
ALTER TABLE sections DROP CONSTRAINT sections_course_sec_track_key;
ALTER TABLE sections ADD CONSTRAINT sections_teaching_course_id_sec_no_key UNIQUE (teaching_course_id,sec_no);
ALTER TABLE teaching_courses DROP COLUMN reg_source;
ALTER TABLE schedule_imports DROP COLUMN review_data;

ALTER TABLE sections DROP COLUMN reg_reserved_for;
ALTER TABLE sections DROP COLUMN reg_pending_schedule;
ALTER TABLE sections DROP COLUMN reg_pending_reason;
