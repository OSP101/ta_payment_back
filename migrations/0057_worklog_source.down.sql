-- Reverting loses the origin of every row, including the ones recorded correctly
-- at insert time after this migration shipped. There is no way to recover it: the
-- heuristic in the up-migration can only be applied to rows whose note is still
-- untouched, and it was never accurate enough to be a substitute for the column.
DROP INDEX IF EXISTS work_logs_assignment_source_idx;

ALTER TABLE work_logs
    DROP COLUMN IF EXISTS source;
