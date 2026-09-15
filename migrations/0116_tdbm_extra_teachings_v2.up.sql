-- TDBM shipped course_code + owner_teacher_name on /extra-teachings (2026-09-14,
-- API_DOCUMENTATION.md) — the fix we asked for in docs/TDBM-API-requirements.md's
-- follow-up: previously this feed carried only raw internal ids (teacher_id,
-- class_id, teaching_id) with nothing to match against our own data, so
-- tdbm_extra_teachings (migration 0097) could only mirror them unresolved.
--
-- The new response also DROPPED every field this table had beyond the ones
-- kept below: detail, status, teacher_id, holiday_id, teaching_id, class_id,
-- dbm_id, etdoc_id, created_user_id, and both timestamps. Table structure
-- follows the response exactly rather than keeping now-dead columns around.
--
-- tdbm_teachers is dropped outright, not just unused: it existed for exactly
-- one purpose — resolving extra_teaching.teacher_id to a name — and
-- owner_teacher_name now arrives pre-resolved. Confirmed nothing else in the
-- codebase reads it.
DROP TABLE IF EXISTS tdbm_teachers;

ALTER TABLE tdbm_extra_teachings
    DROP COLUMN IF EXISTS detail,
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS teacher_id,
    DROP COLUMN IF EXISTS holiday_id,
    DROP COLUMN IF EXISTS teaching_id,
    DROP COLUMN IF EXISTS class_id,
    DROP COLUMN IF EXISTS dbm_id,
    DROP COLUMN IF EXISTS etdoc_id,
    DROP COLUMN IF EXISTS created_user_id,
    DROP COLUMN IF EXISTS tdbm_created_at,
    DROP COLUMN IF EXISTS tdbm_updated_at;

ALTER TABLE tdbm_extra_teachings
    ADD COLUMN course_code       TEXT,
    ADD COLUMN owner_teacher_name TEXT,
    -- Resolved match against teaching_courses, computed by
    -- TDBMService.resolveCourseMatches on every sync — NOT trusted from TDBM
    -- directly. course_code identifies the SUBJECT only; TDBM has no section
    -- number, so a course with more than one section still cannot be narrowed
    -- further than "this teaching_course" — staff judgement picks the section.
    ADD COLUMN teaching_course_id UUID REFERENCES teaching_courses(id);

DROP INDEX IF EXISTS idx_tdbm_extra_teachings_teacher;
CREATE INDEX idx_tdbm_extra_teachings_course_code ON tdbm_extra_teachings (course_code);
CREATE INDEX idx_tdbm_extra_teachings_teaching_course ON tdbm_extra_teachings (teaching_course_id);

-- Sync log gains a match-rate column, extra-teachings only (NULL for holidays
-- rows) — otherwise "did the new course_code actually match our courses"
-- would only be answerable by querying tdbm_extra_teachings live, and a bad
-- match rate on a past run would leave no trace once the data moves on.
ALTER TABLE tdbm_sync_log
    ADD COLUMN matched INT;
