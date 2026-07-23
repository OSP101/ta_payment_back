-- 0029_submission_flow_no_signatures.up.sql
-- The monthly workflow no longer models digital "signatures": finance only
-- accepts wet-ink signatures on the printed export, so the TA-confirm and
-- lecturer-sign steps were meaningless ceremony. The lecturer's DAILY worklog
-- approval is the real review. New per-(TA × month × course) staff lifecycle:
--
--     pending  --(all daily worklog approved: derived "รอเจ้าหน้าที่")-->
--     exported --(staff exported the file; worklog LOCKED here)-->
--     finance_sent  (staff confirmed the paperwork reached finance)
--
-- Lock point moves from finance_sent (0028) to exported.

ALTER TABLE submission_period_status
    ADD COLUMN IF NOT EXISTS exported_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS exported_by   UUID REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS exported_name TEXT;

-- Collapse the removed stages onto the new machine. ta_signed/lecturer_signed
-- were pre-staff ceremony with no payout meaning -> back to pending. submitted
-- meant "staff reviewed, pre-finance" -> map to the new lock state 'exported'
-- so any in-flight row stays frozen rather than silently unlocking.
UPDATE submission_period_status SET status = 'pending'
    WHERE status IN ('ta_signed', 'lecturer_signed');
UPDATE submission_period_status
    SET status = 'exported',
        exported_at   = COALESCE(exported_at, submitted_at, now()),
        exported_by   = COALESCE(exported_by, staff_reviewed_by),
        exported_name = COALESCE(exported_name, staff_reviewed_name)
    WHERE status = 'submitted';

ALTER TABLE submission_period_status
    DROP CONSTRAINT IF EXISTS submission_period_status_status_check;
ALTER TABLE submission_period_status
    ADD CONSTRAINT submission_period_status_status_check
    CHECK (status IN ('pending', 'exported', 'finance_sent', 'skipped'));

-- The worklog lock lookup now covers BOTH frozen states (0028 indexed only
-- finance_sent). Replace the partial index accordingly.
DROP INDEX IF EXISTS submission_period_status_finance_lock_idx;
CREATE INDEX IF NOT EXISTS submission_period_status_lock_idx
    ON submission_period_status (ta_id, teaching_course_id)
    WHERE status IN ('exported', 'finance_sent');
