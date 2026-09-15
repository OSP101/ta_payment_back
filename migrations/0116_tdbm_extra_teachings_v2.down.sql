ALTER TABLE tdbm_sync_log
    DROP COLUMN IF EXISTS matched;

DROP INDEX IF EXISTS idx_tdbm_extra_teachings_teaching_course;
DROP INDEX IF EXISTS idx_tdbm_extra_teachings_course_code;

ALTER TABLE tdbm_extra_teachings
    DROP COLUMN IF EXISTS teaching_course_id,
    DROP COLUMN IF EXISTS owner_teacher_name,
    DROP COLUMN IF EXISTS course_code;

ALTER TABLE tdbm_extra_teachings
    ADD COLUMN detail           TEXT,
    ADD COLUMN status           TEXT,
    ADD COLUMN teacher_id       INT,
    ADD COLUMN holiday_id       INT,
    ADD COLUMN teaching_id      INT,
    ADD COLUMN class_id         INT,
    ADD COLUMN dbm_id           INT,
    ADD COLUMN etdoc_id         INT,
    ADD COLUMN created_user_id  INT,
    ADD COLUMN tdbm_created_at  TIMESTAMPTZ,
    ADD COLUMN tdbm_updated_at  TIMESTAMPTZ;

CREATE INDEX idx_tdbm_extra_teachings_teacher ON tdbm_extra_teachings (teacher_id);

CREATE TABLE tdbm_teachers (
    teacher_id      INT PRIMARY KEY,
    prefix          TEXT,
    "position"      TEXT,
    degree          TEXT,
    name            TEXT NOT NULL,
    email           TEXT,
    account_user_id INT,
    tdbm_created_at TIMESTAMPTZ,
    tdbm_updated_at TIMESTAMPTZ,
    synced_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
