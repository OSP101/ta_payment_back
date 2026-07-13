-- 0020_export_batches.down.sql
DROP INDEX IF EXISTS export_batches_period_idx;
DROP INDEX IF EXISTS export_batches_course_idx;
DROP TABLE IF EXISTS export_batches;
