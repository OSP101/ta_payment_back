DROP TRIGGER IF EXISTS work_logs_hours_from_times ON work_logs;
DROP FUNCTION IF EXISTS work_log_hours_from_times();
-- Narrowing back rounds every value to 2 decimals (the pre-0133 behaviour).
ALTER TABLE work_logs ALTER COLUMN hours TYPE NUMERIC(4,2) USING round(hours, 2);
