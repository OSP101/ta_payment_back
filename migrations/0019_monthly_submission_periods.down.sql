-- 0019_monthly_submission_periods.down.sql
DROP INDEX IF EXISTS submission_period_status_ta_idx;
DROP TABLE IF EXISTS submission_period_status;
DROP INDEX IF EXISTS submission_periods_due_idx;
DROP TABLE IF EXISTS submission_periods;
