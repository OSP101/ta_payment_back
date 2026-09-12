ALTER TABLE pay_rates
  DROP COLUMN IF EXISTS plan_students_per_ta,
  DROP COLUMN IF EXISTS plan_min_students_per_ta,
  DROP COLUMN IF EXISTS plan_suggested_ta_cap;
