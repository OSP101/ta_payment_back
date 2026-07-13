-- 0018_business_rules_2026.up.sql
-- Aligns rate/cap fields with ประกาศ 731/2565 + 1080/2565 and Q&A confirmations
-- (see plans/q-a-recursive-goose.md). Introduces per-track daily hour caps,
-- daily baht cap, grad-regular hourly rate, grad-special term cap, and the
-- work_logs.parent_kind column that binds "อื่นๆ" entries to lecture/lab.

ALTER TABLE pay_rates
  ADD COLUMN IF NOT EXISTS graduate_regular_hourly    NUMERIC(10,2) NOT NULL DEFAULT 50,
  ADD COLUMN IF NOT EXISTS grad_special_term_cap      NUMERIC(10,2) NOT NULL DEFAULT 12000,
  ADD COLUMN IF NOT EXISTS daily_pay_cap_baht         NUMERIC(10,2) NOT NULL DEFAULT 300,
  ADD COLUMN IF NOT EXISTS ug_regular_daily_hour_cap  NUMERIC(4,2)  NOT NULL DEFAULT 7,
  ADD COLUMN IF NOT EXISTS ug_special_daily_hour_cap  NUMERIC(4,2)  NOT NULL DEFAULT 6,
  ADD COLUMN IF NOT EXISTS grad_regular_daily_hour_cap NUMERIC(4,2) NOT NULL DEFAULT 6;

-- Bring the latest pay_rates row in line with the announcement (40/50/50/4000).
-- Idempotent: if these values are already set (e.g. seed) the UPDATE is a no-op.
UPDATE pay_rates SET
  undergrad_regular        = 40,
  undergrad_special        = 50,
  graduate_regular_hourly  = 50,
  graduate_special_lumpsum = 4000
WHERE effective_from = (SELECT MAX(effective_from) FROM pay_rates);

-- parent_kind ties an activity='other' work_log row to its parent session type
-- so per-session credit-hour caps can be enforced (Q&A rule 2).
ALTER TABLE work_logs
  ADD COLUMN IF NOT EXISTS parent_kind TEXT
    CHECK (parent_kind IS NULL OR parent_kind IN ('lecture','lab'));

-- Backfill legacy 'other' rows with a safe default so the new CHECK doesn't
-- reject existing data. Choosing 'lecture' is arbitrary; staff can retag.
UPDATE work_logs SET parent_kind = 'lecture'
 WHERE activity = 'other' AND parent_kind IS NULL;

ALTER TABLE work_logs
  ADD CONSTRAINT work_logs_other_needs_parent_kind
  CHECK (activity <> 'other' OR parent_kind IN ('lecture','lab'));

CREATE INDEX IF NOT EXISTS work_logs_assignment_date_idx
  ON work_logs (assignment_id, work_date);
