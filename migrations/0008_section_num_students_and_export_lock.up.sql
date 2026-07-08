-- Per-section student counts + export lock timestamp.
--
-- Motivation:
--   1) Lecturers now specify num_students per section (not just aggregate).
--   2) Once staff exports the course package, the section list + counts
--      snapshot into that export and must not drift, so we mark exported_at
--      and gate later edits on it.
ALTER TABLE sections
    ADD COLUMN IF NOT EXISTS num_students INT NOT NULL DEFAULT 0;

ALTER TABLE teaching_courses
    ADD COLUMN IF NOT EXISTS exported_at TIMESTAMPTZ;
