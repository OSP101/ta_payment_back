-- 0029_submission_flow_no_signatures.down.sql
-- Reverse 0029: restore the six-state signature machine and the finance-only
-- lock index. 'exported' rows map back to 'submitted' (the closest old state);
-- the exported_* snapshot columns are dropped.

UPDATE submission_period_status
    SET status = 'submitted',
        submitted_at        = COALESCE(submitted_at, exported_at),
        staff_reviewed_by   = COALESCE(staff_reviewed_by, exported_by),
        staff_reviewed_name = COALESCE(staff_reviewed_name, exported_name)
    WHERE status = 'exported';

ALTER TABLE submission_period_status
    DROP CONSTRAINT IF EXISTS submission_period_status_status_check;
ALTER TABLE submission_period_status
    ADD CONSTRAINT submission_period_status_status_check
    CHECK (status IN ('pending','ta_signed','lecturer_signed','submitted','finance_sent','skipped'));

DROP INDEX IF EXISTS submission_period_status_lock_idx;
CREATE INDEX IF NOT EXISTS submission_period_status_finance_lock_idx
    ON submission_period_status (ta_id, teaching_course_id)
    WHERE status = 'finance_sent';

ALTER TABLE submission_period_status
    DROP COLUMN IF EXISTS exported_at,
    DROP COLUMN IF EXISTS exported_by,
    DROP COLUMN IF EXISTS exported_name;
