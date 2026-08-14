-- Irreversible by design: the backfill replaced NULLs that carried no
-- information of their own, and there is no record of which rows were NULL
-- before it ran. Blanking every months value to undo it would destroy the real
-- month lists written by the handler alongside them, so the down migration
-- restores only the column comment.
COMMENT ON COLUMN export_batches.months IS
    'Gregorian YYYY-MM keys this ZIP covered, e.g. {2026-06,2026-07,2026-08,2026-09}. NULL for batches predating the fiscal-year split, which covered the whole term.';
