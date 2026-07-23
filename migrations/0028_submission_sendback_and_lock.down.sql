-- 0028_submission_sendback_and_lock.down.sql

DROP INDEX IF EXISTS submission_period_status_finance_lock_idx;

ALTER TABLE submission_period_status
    DROP COLUMN IF EXISTS sent_back_at,
    DROP COLUMN IF EXISTS sent_back_by,
    DROP COLUMN IF EXISTS sent_back_name,
    DROP COLUMN IF EXISTS sent_back_reason;
