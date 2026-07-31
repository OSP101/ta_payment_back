-- Reverses 0041. The workload rows created by the backfill are NOT removed:
-- once the application writes per-section hours they are real data, and there
-- is no way to tell a backfilled row from one a lecturer typed. Dropping the
-- columns is enough to restore the old read paths, which LEFT JOIN the form
-- and simply see one row per assignment instead of one per (request, TA).

DROP INDEX IF EXISTS ta_request_assignments_ta_state_idx;

ALTER TABLE ta_request_assignments
    DROP COLUMN IF EXISTS cotaught_group,
    DROP COLUMN IF EXISTS state_decided_at,
    DROP COLUMN IF EXISTS state_reason,
    DROP COLUMN IF EXISTS state;

DROP TYPE IF EXISTS ta_assignment_state;
