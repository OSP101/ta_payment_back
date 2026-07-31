-- Staff corrections to a TA's month become an accountable act.
--
-- Until now a staff edit went through StaffUpsert / StaffDelete: the row changed,
-- an audit line said "เจ้าหน้าที่แก้ไขบันทึกเวลา", and that was the whole record.
-- Nobody could answer WHY, the lecturer who approved the original hours was
-- never told, and there was nowhere to put the evidence the officer was looking
-- at when they decided the hours were wrong.
--
-- A batch is one officer's corrections to one (TA, course, month), committed
-- together behind their password, carrying one reason and up to three images.
-- Batching rather than per-row is deliberate: an officer fixing five rows has
-- ONE reason ("ตารางสอนเปลี่ยนกลางเทอม"), and asking five times teaches them to
-- type "แก้ไข" five times instead of explaining once.
CREATE TABLE worklog_edit_batches (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    teaching_course_id UUID NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    ta_id              UUID NOT NULL REFERENCES users(id),
    -- The month the corrections belong to, as 'YYYY-MM' in the Gregorian year
    -- that work_logs.work_date uses. Kept denormalised so the history survives a
    -- submission period being re-cut.
    year_month         TEXT NOT NULL,
    actor_id           UUID NOT NULL REFERENCES users(id),
    reason             TEXT NOT NULL CHECK (length(btrim(reason)) >= 10),
    -- One object per row touched: {work_log_id, action, before, after}. Stored as
    -- a document rather than a child table because it is never queried by field —
    -- it is read back whole, by a human, next to the reason that explains it.
    changes            JSONB NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX worklog_edit_batches_course_ta_month_idx
    ON worklog_edit_batches (teaching_course_id, ta_id, year_month, created_at DESC);

-- Evidence images. Separate table rather than a jsonb array because these are
-- real files in the object store: deleting a batch must be able to find its keys
-- to purge them, and a FK does that where a buried array cannot.
CREATE TABLE worklog_edit_files (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id    UUID NOT NULL REFERENCES worklog_edit_batches(id) ON DELETE CASCADE,
    storage_key TEXT NOT NULL,
    filename    TEXT NOT NULL,
    size_bytes  BIGINT NOT NULL,
    mime        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX worklog_edit_files_batch_idx ON worklog_edit_files (batch_id);
