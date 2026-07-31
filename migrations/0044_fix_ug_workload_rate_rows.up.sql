-- Correct the undergrad workload rate on EXISTING pay_rates rows.
--
-- Migration 0005 already identified this: it set the column DEFAULT to 300 and
-- documented the faculty workbook's formula (ชีต "2_59 ป.ตรี"):
--
--   ค่า TA/เดือน = ภาระงาน/สัปดาห์ × (50% × 200 ตรี + 50% × 400 บัณฑิต)
--                = ภาระงาน/สัปดาห์ × 300
--
-- But ALTER COLUMN ... SET DEFAULT only affects future inserts that OMIT the
-- column, and cmd/seed names it explicitly. So the default never applied and
-- every seeded database kept 200 — a ceiling one third below what the faculty
-- actually funds.
--
-- This is not cosmetic. BudgetService sets PerCourseMaxBaht = TermPay, and
-- ExportService uses that as the prorata cap, so an under-stated ceiling
-- silently cuts TA payouts the faculty had budgeted for. Verified against the
-- workbook: 322238 3(3-0-6) with 69 students = 12,420฿, which the formula
-- reproduces exactly at 300 and misses by 4,140฿ at 200.
--
-- Only rows still carrying the stale 200 are touched. A rate someone set
-- deliberately to anything else is left alone.
UPDATE pay_rates
SET ug_workload_rate_regular = 300
WHERE ug_workload_rate_regular = 200;

-- The backend reads ug_workload_rate_regular for BOTH tracks — under the
-- workbook's model ภาคปกติ and ภาคพิเศษ share the effective rate and differ
-- only in student count. Align the unused column so a future reader does not
-- take 250 for a live figure.
UPDATE pay_rates
SET ug_workload_rate_special = 300
WHERE ug_workload_rate_special = 250;
