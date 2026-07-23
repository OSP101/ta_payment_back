-- 0028_submission_sendback_and_lock.up.sql
-- Two hardening features for the monthly submission workflow:
--   1. "ตีกลับ" (send-back): staff/lecturer can push a status row back to an
--      earlier step with a mandatory reason. The sent_back_* columns snapshot
--      who bounced it, when, and why so the timeline can render the event.
--   2. finance_sent lock: work_logs whose month has reached finance_sent are
--      frozen for every role. The partial index makes the per-write lock
--      lookup (ta_id × teaching_course_id × finance_sent) cheap.

ALTER TABLE submission_period_status
    ADD COLUMN IF NOT EXISTS sent_back_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS sent_back_by     UUID REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS sent_back_name   TEXT,
    ADD COLUMN IF NOT EXISTS sent_back_reason TEXT;

CREATE INDEX IF NOT EXISTS submission_period_status_finance_lock_idx
    ON submission_period_status (ta_id, teaching_course_id)
    WHERE status = 'finance_sent';
