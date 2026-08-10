-- 10/08/2026 — the per-course claim ZIP (ใบเบิก) can now cover part of a term,
-- for the same reason ปะหน้าจ่ายตรง can (migration 0079): งบแผ่นดิน closes
-- 30 กันยายน while ภาคต้น teaches มิ.ย.–ต.ค., and the two halves are claimed
-- against different appropriations on separate documents.
--
-- Until now the claim workbook was built by claimLogsAllMonths — literally
-- "claimLogs without the month filter" — so a second export issued after
-- October repeated มิ.ย.–ก.ย. in full and the finance office was billed for
-- them twice. months records which Gregorian 'YYYY-MM' a batch actually
-- covered, so the export history can say which slice a file was and the
-- screen can show which months still have no claim.
--
-- Gregorian, matching work_logs.work_date — NOT submission_periods.year_month,
-- which carries a Buddhist academic year ('2569-06') for the same month.
--
-- Nullable: batches written before this column existed covered the whole term,
-- and an empty array would claim they covered nothing.
ALTER TABLE export_batches
    ADD COLUMN IF NOT EXISTS months TEXT[];

COMMENT ON COLUMN export_batches.months IS
    'Gregorian YYYY-MM keys this ZIP covered, e.g. {2026-06,2026-07,2026-08,2026-09}. NULL for batches predating the fiscal-year split, which covered the whole term.';
