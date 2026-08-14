-- 13/08/2026 — make export_batches.months mean one thing.
--
-- 0080 added the column and documented NULL as "predates the split, covered the
-- whole term". The write path never caught up: the ZIP handler passed the raw
-- ?months= query parameter straight through, and an absent parameter — the
-- normal case, since the month picker is optional — reached the ledger as nil
-- and was stored as NULL. So NULL meant two different things, and its two
-- readers each picked one:
--
--   * CourseExportCoverage reads NULL as covering every month of the term;
--   * the fiscal-round predicates test `months && $1`, and `NULL && anything`
--     is NULL rather than true, so NULL covers nothing.
--
-- The visible damage was on the round-2 side: a whole-term ZIP that really did
-- claim ตุลาคม left its course flagged "เหลือรอบ 2" forever on the payouts list
-- and stuck in unexported_courses on the round-2 progress board, with no action
-- available that could ever clear it.
--
-- The handler now resolves the selection before recording it, so new rows carry
-- their real month list. This backfills the existing ones with the same value
-- the old NULL was always supposed to stand for — every month of the batch's
-- own term — which is what CourseExportCoverage has been reporting all along.
-- After this, NULL survives only where a term genuinely has no submission
-- periods to enumerate, and both readers agree on the rows that do have months.
--
-- submission_periods.year_month is Buddhist-academic ('2569-06'); work_logs and
-- this column are Gregorian ('2026-06'). Academic year Y opens in มิถุนายน of
-- Y-543 and runs to พฤษภาคม of the following year, so มิ.ย.–ธ.ค. map to Y-543
-- and ม.ค.–พ.ค. wrap to Y-543+1 — the same rule as gregorianYearMonth() in
-- term_months.go, kept in step with it deliberately.
UPDATE export_batches eb
SET months = tm.months
FROM (
    SELECT tc.id AS teaching_course_id,
           array_agg(
               to_char(
                   (split_part(sp.year_month, '-', 1)::int - 543
                       + CASE WHEN split_part(sp.year_month, '-', 2)::int <= 5
                              THEN 1 ELSE 0 END),
                   'FM0000')
               || '-' || split_part(sp.year_month, '-', 2)
               ORDER BY
                   (split_part(sp.year_month, '-', 1)::int - 543
                       + CASE WHEN split_part(sp.year_month, '-', 2)::int <= 5
                              THEN 1 ELSE 0 END),
                   split_part(sp.year_month, '-', 2)::int
           ) AS months
    FROM teaching_courses tc
    JOIN submission_periods sp ON sp.term_id = tc.term_id
    GROUP BY tc.id
) tm
WHERE eb.teaching_course_id = tm.teaching_course_id
  AND eb.months IS NULL;

COMMENT ON COLUMN export_batches.months IS
    'Gregorian YYYY-MM keys this ZIP covered, e.g. {2026-06,2026-07,2026-08,2026-09}. Always populated since migration 0083; NULL now only where the term has no submission periods to enumerate.';
