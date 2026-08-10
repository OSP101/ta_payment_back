-- 10/08/2026 — ปะหน้าจ่ายตรง can now cover part of a term rather than all of it.
--
-- Why: the Thai government budget year closes 30 September, but ภาคต้น teaches
-- มิ.ย.–ต.ค. Work up to กันยายน is claimed against the old year's appropriation
-- and ตุลาคม against the new one, on SEPARATE documents. Until now this table
-- recorded one generation per TERM, so two files issued for one term were
-- indistinguishable — and since the builder always summed the whole term, the
-- second file silently repeated the first file's money.
--
-- months holds the Gregorian 'YYYY-MM' keys the file actually covered, matching
-- SlotSettlement.YearMonth (work_logs.work_date's own calendar), NOT
-- submission_periods.year_month, which is a BUDDHIST academic year and would
-- read '2569-06' for the same month.
--
-- Nullable rather than NOT NULL DEFAULT '{}': rows written before this column
-- existed covered the whole term, and an empty array would claim they covered
-- nothing. NULL keeps "not recorded — treat as the whole term" honest and
-- distinguishable from a genuinely empty selection, which the service refuses
-- to write anyway.
ALTER TABLE transfer_cover_exports
    ADD COLUMN IF NOT EXISTS months TEXT[];

COMMENT ON COLUMN transfer_cover_exports.months IS
    'Gregorian YYYY-MM keys this generation covered, e.g. {2026-06,2026-07,2026-08,2026-09}. NULL for rows predating the fiscal-year split, which covered the whole term.';
