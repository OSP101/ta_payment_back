DROP INDEX IF EXISTS idx_tdbm_extra_original;
ALTER TABLE tdbm_extra_teachings
 DROP COLUMN source_teaching_id, DROP COLUMN source_class_id, DROP COLUMN source_holiday_id,
 DROP COLUMN source_original_date, DROP COLUMN source_original_start, DROP COLUMN source_original_end,
 DROP COLUMN source_original_kind, DROP COLUMN source_latest_status_id, DROP COLUMN source_updated_at,
 DROP COLUMN source_status_updated_at, DROP COLUMN source_missing, DROP COLUMN reconciliation_error;
