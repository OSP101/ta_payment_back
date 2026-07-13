-- 0020_export_batches.up.sql
-- Phase 4: every time staff hits POST /exports/course/:id, we persist a record
-- of the resulting zip so the exports dashboard can list history and re-issue
-- downloads without re-computing the (potentially expensive) build.

CREATE TABLE IF NOT EXISTS export_batches (
    id                   UUID PRIMARY KEY,
    teaching_course_id   UUID NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    submission_period_id UUID REFERENCES submission_periods(id) ON DELETE SET NULL,
    file_path            TEXT NOT NULL,        -- storage.Store-relative path
    file_name            TEXT NOT NULL,
    ta_count             INT NOT NULL DEFAULT 0,
    total_baht           NUMERIC(12,2) NOT NULL DEFAULT 0,
    generated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    generated_by         UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS export_batches_course_idx
    ON export_batches (teaching_course_id, generated_at DESC);
CREATE INDEX IF NOT EXISTS export_batches_period_idx
    ON export_batches (submission_period_id);
