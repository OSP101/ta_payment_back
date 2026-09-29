-- TDBM is the sole automatic source of holidays (router.go, 2026-09-15). The
-- 'national' rows seeded by 0024 (and any left behind by the removed BOT sync)
-- duplicated TDBM's own entries under a different name — e.g. 13 Oct showed
-- both "วันคล้ายวันสวรรคต ร.9" (seed) and "วันนวมินทรมหาราช" (TDBM).
--
-- Only machine-inserted rows go: created_by IS NULL. Anything staff typed in
-- by hand keeps its row. Manual entry is now limited to faculty/custom
-- (validHolidaySource), so no new 'national' rows appear.
DELETE FROM public_holidays
WHERE source = 'national'
  AND created_by IS NULL;
