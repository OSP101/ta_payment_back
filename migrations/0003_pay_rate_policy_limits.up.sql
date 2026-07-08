-- Additional policy limits from the official TA-payment regulation notes:
--   * Undergrad regular: max 7 hours/day
--   * Any student: TA in at most 3 courses concurrently
-- These are advisory limits used for validation & display; not part of the
-- monetary formula.
ALTER TABLE pay_rates ADD COLUMN IF NOT EXISTS ug_max_hours_per_day    INT NOT NULL DEFAULT 7;
ALTER TABLE pay_rates ADD COLUMN IF NOT EXISTS max_courses_per_student INT NOT NULL DEFAULT 3;
