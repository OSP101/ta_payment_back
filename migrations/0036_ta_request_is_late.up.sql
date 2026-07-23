-- TA requests may now be submitted after the request window's closes_at
-- instead of being blocked. Such requests are accepted but flagged late so
-- staff can see they arrived past the deadline.
ALTER TABLE ta_requests
    ADD COLUMN IF NOT EXISTS is_late BOOLEAN NOT NULL DEFAULT FALSE;
