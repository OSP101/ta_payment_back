-- Reverses 0045. Restores the column at the effective rate rather than the old
-- 250: no code reads it, and re-creating it with a value that disagrees with
-- ug_workload_rate_regular would recreate the exact confusion the drop removed.

ALTER TABLE pay_rates
    ADD COLUMN IF NOT EXISTS ug_workload_rate_special NUMERIC(10,2) NOT NULL DEFAULT 300;
