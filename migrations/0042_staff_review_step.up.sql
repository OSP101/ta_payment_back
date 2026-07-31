-- Phase 3 of the 24/07/2026 meeting plan: staff review becomes a state, not a
-- habit.
--
-- The monthly lifecycle gains a step between the lecturer's approval and the
-- payout export:
--
--   pending → staff_reviewed → exported → finance_sent
--
-- Before this, export accepted any month the lecturer had approved. Staff did
-- check the numbers, but nothing recorded that they had, so a month could reach
-- finance without review and there was no answer to "who checked this?" after
-- the money moved.
--
-- Only the CHECK constraint changes. The columns this step writes —
-- staff_reviewed_by, staff_reviewed_name, staff_comment — have existed on the
-- table since 0026 and were never written by any code path; they were
-- scaffolding waiting for this step.

ALTER TABLE submission_period_status
    DROP CONSTRAINT IF EXISTS submission_period_status_status_check;

ALTER TABLE submission_period_status
    ADD CONSTRAINT submission_period_status_status_check
    CHECK (status = ANY (ARRAY[
        'pending'::text,
        'staff_reviewed'::text,
        'exported'::text,
        'finance_sent'::text,
        'skipped'::text
    ]));

-- The review queue and the export gate both filter on (status) within a term,
-- and the queue runs on every page load of staff step 3.
CREATE INDEX IF NOT EXISTS submission_period_status_status_idx
    ON submission_period_status (status);

COMMENT ON COLUMN submission_period_status.staff_reviewed_by IS
    'Staff member who signed off this month for export (step 3, ตรวจสอบเบิกจ่ายค่าตอบแทน). NULL until reviewed.';
