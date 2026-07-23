-- 0026_submission_period_timeline.up.sql
-- Extends submission_period_status so the UI can render an approval-style
-- timeline (see screenshot from user 2026-07-15): each step shows who signed,
-- when, and any comment they left. Also adds a 4th "finance_sent" state that
-- staff toggles manually once the batch has been handed to the finance office.

ALTER TABLE submission_period_status
    ADD COLUMN IF NOT EXISTS ta_signed_name        TEXT,
    ADD COLUMN IF NOT EXISTS lecturer_signed_by    UUID REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS lecturer_signed_name  TEXT,
    ADD COLUMN IF NOT EXISTS lecturer_comment      TEXT,
    ADD COLUMN IF NOT EXISTS staff_reviewed_by     UUID REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS staff_reviewed_name   TEXT,
    ADD COLUMN IF NOT EXISTS staff_comment         TEXT,
    ADD COLUMN IF NOT EXISTS finance_sent_at       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS finance_sent_by       UUID REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS finance_sent_name     TEXT,
    ADD COLUMN IF NOT EXISTS finance_note          TEXT;

ALTER TABLE submission_period_status
    DROP CONSTRAINT IF EXISTS submission_period_status_status_check;

ALTER TABLE submission_period_status
    ADD CONSTRAINT submission_period_status_status_check
    CHECK (status IN ('pending','ta_signed','lecturer_signed','submitted','finance_sent','skipped'));
