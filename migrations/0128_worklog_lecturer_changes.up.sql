-- What a lecturer (or staff) changed on a TA's work log while it was waiting
-- for review, and why.
--
-- The lecturer can now correct a submitted row — shorten it, move it — or cut
-- it outright instead of bouncing the whole month back to the TA. Either way
-- the TA must be able to see what was changed and the reason, and anyone
-- checking a claim later must be able to trace it. audit_logs has the diff but
-- is not something the TA reads, and a cut row is gone from work_logs, so it
-- could not carry its own reason.
--
-- A cut row is DELETED from work_logs rather than flagged: every billing,
-- budget, cap and export query reads work_logs, and a new status would have to
-- be taught to each of them. Its last state lives on here as `before`.
--
-- One row per work_log touched. A co-taught sitting is written against every
-- section it serves, so correcting it writes one row per copy — each
-- assignment carries its own history.
CREATE TABLE IF NOT EXISTS work_log_lecturer_changes (
    id            BIGSERIAL PRIMARY KEY,
    assignment_id UUID NOT NULL REFERENCES ta_request_assignments(id) ON DELETE CASCADE,
    -- No FK: after a cut the row it names no longer exists.
    work_log_id   UUID NOT NULL,
    action        TEXT NOT NULL CHECK (action IN ('edit', 'cut')),
    work_date     DATE NOT NULL,
    before        JSONB NOT NULL,
    after         JSONB,          -- NULL for a cut
    reason        TEXT NOT NULL CHECK (length(btrim(reason)) > 0),
    actor_id      UUID REFERENCES users(id) ON DELETE SET NULL,
    actor_name    TEXT NOT NULL DEFAULT '',
    actor_role    TEXT NOT NULL CHECK (actor_role IN ('lecturer', 'staff')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS work_log_lecturer_changes_assignment_idx
    ON work_log_lecturer_changes (assignment_id, created_at DESC);
CREATE INDEX IF NOT EXISTS work_log_lecturer_changes_log_idx
    ON work_log_lecturer_changes (work_log_id);
