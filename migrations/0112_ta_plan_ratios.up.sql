-- TA planning ratios, editable by staff instead of living in the planner's code.
--   plan_students_per_ta      guideline: one TA per this many students (นศ./25)
--   plan_min_students_per_ta  density ceiling: never plan more than one TA per this many
--   plan_suggested_ta_cap     the guideline's headcount cap (0 = no cap)
ALTER TABLE pay_rates
  ADD COLUMN IF NOT EXISTS plan_students_per_ta     INTEGER NOT NULL DEFAULT 25,
  ADD COLUMN IF NOT EXISTS plan_min_students_per_ta INTEGER NOT NULL DEFAULT 15,
  ADD COLUMN IF NOT EXISTS plan_suggested_ta_cap    INTEGER NOT NULL DEFAULT 3;
