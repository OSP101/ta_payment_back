-- REG legitimately repeats SEC 01 for regular and special tracks.
ALTER TABLE sections DROP CONSTRAINT sections_teaching_course_id_sec_no_key;
ALTER TABLE sections ADD CONSTRAINT sections_course_sec_track_key UNIQUE (teaching_course_id, sec_no, track);
-- Preserve source fields that do not have a dedicated operational column,
-- including reserved groups, prerequisites, exams and pending timetable lines.
ALTER TABLE teaching_courses ADD COLUMN reg_source JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE schedule_imports ADD COLUMN review_data JSONB;

ALTER TABLE sections ADD COLUMN reg_reserved_for TEXT NOT NULL DEFAULT '';
ALTER TABLE sections ADD COLUMN reg_pending_schedule BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE sections ADD COLUMN reg_pending_reason TEXT NOT NULL DEFAULT '';
