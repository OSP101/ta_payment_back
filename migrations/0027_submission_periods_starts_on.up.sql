-- 0027_submission_periods_starts_on.up.sql
-- Adds an explicit opening date to submission_periods so each monthly cycle
-- is a proper [starts_on, due_date] window. Backfills existing rows to the
-- first day of their year_month so TAs can still sign historical periods.

ALTER TABLE submission_periods
    ADD COLUMN IF NOT EXISTS starts_on DATE;

UPDATE submission_periods
SET starts_on = TO_DATE(
    -- year_month is stored as Buddhist "YYYY-MM"; convert to Gregorian for
    -- the DATE column (Postgres treats DATE as proleptic Gregorian).
    (SPLIT_PART(year_month,'-',1)::int - 543) || '-' || SPLIT_PART(year_month,'-',2) || '-01',
    'YYYY-MM-DD')
WHERE starts_on IS NULL;

ALTER TABLE submission_periods
    ALTER COLUMN starts_on SET NOT NULL;
