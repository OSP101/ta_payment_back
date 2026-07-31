-- Reverses 0042. Any row sitting in 'staff_reviewed' is moved back to
-- 'pending' first, otherwise re-adding the narrower constraint would fail.
-- Moving them back (rather than forward to 'exported') is the safe direction:
-- it re-queues the month for review instead of asserting a payout was made.

UPDATE submission_period_status
SET status = 'pending'
WHERE status = 'staff_reviewed';

DROP INDEX IF EXISTS submission_period_status_status_idx;

ALTER TABLE submission_period_status
    DROP CONSTRAINT IF EXISTS submission_period_status_status_check;

ALTER TABLE submission_period_status
    ADD CONSTRAINT submission_period_status_status_check
    CHECK (status = ANY (ARRAY[
        'pending'::text,
        'exported'::text,
        'finance_sent'::text,
        'skipped'::text
    ]));
