-- Drop pay_rates.ug_workload_rate_special.
--
-- Migration 0005 kept it "เพื่อ backward-compat" after the budget calculation
-- stopped reading it. Nine migrations later nothing has read it since: the
-- faculty workbook (ชีต "2_59 ป.ตรี") applies ONE effective rate to both
-- tracks — ภาคปกติ and ภาคพิเศษ differ only in student count, never in rate.
--
-- Leaving it was not free. It still appeared in the settings form and in the
-- pay-rate API, so anyone reading the schema had to work out for themselves
-- that a column named "special rate" is not the rate used for special-track
-- sections. It also sat at 250 while the real rate was 300, which reads as a
-- deliberate distinction rather than the dead value it is.
--
-- The regular rate column keeps its name for continuity; the code comments at
-- BudgetService document that it applies to both tracks.

ALTER TABLE pay_rates DROP COLUMN IF EXISTS ug_workload_rate_special;
