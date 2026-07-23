-- Reverse of 0026: drop the extra columns and restore the original CHECK.
ALTER TABLE submission_period_status
    DROP CONSTRAINT IF EXISTS submission_period_status_status_check;

ALTER TABLE submission_period_status
    ADD CONSTRAINT submission_period_status_status_check
    CHECK (status IN ('pending','ta_signed','lecturer_signed','submitted','skipped'));

ALTER TABLE submission_period_status
    DROP COLUMN IF EXISTS finance_note,
    DROP COLUMN IF EXISTS finance_sent_name,
    DROP COLUMN IF EXISTS finance_sent_by,
    DROP COLUMN IF EXISTS finance_sent_at,
    DROP COLUMN IF EXISTS staff_comment,
    DROP COLUMN IF EXISTS staff_reviewed_name,
    DROP COLUMN IF EXISTS staff_reviewed_by,
    DROP COLUMN IF EXISTS lecturer_comment,
    DROP COLUMN IF EXISTS lecturer_signed_name,
    DROP COLUMN IF EXISTS lecturer_signed_by,
    DROP COLUMN IF EXISTS ta_signed_name;
