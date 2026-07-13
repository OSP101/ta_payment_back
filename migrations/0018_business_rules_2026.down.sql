-- 0018_business_rules_2026.down.sql
DROP INDEX IF EXISTS work_logs_assignment_date_idx;
ALTER TABLE work_logs DROP CONSTRAINT IF EXISTS work_logs_other_needs_parent_kind;
ALTER TABLE work_logs DROP COLUMN IF EXISTS parent_kind;

ALTER TABLE pay_rates
  DROP COLUMN IF EXISTS graduate_regular_hourly,
  DROP COLUMN IF EXISTS grad_special_term_cap,
  DROP COLUMN IF EXISTS daily_pay_cap_baht,
  DROP COLUMN IF EXISTS ug_regular_daily_hour_cap,
  DROP COLUMN IF EXISTS ug_special_daily_hour_cap,
  DROP COLUMN IF EXISTS grad_regular_daily_hour_cap;
