-- Reverse 0058. The narrower UNIQUE (holiday_date, source) cannot be restored
-- while two windows share a (date, source), so collapse those to the earliest
-- row first — the alternative is a migration that simply fails to run down.
DROP INDEX IF EXISTS uq_public_holidays_date_source_window;

DELETE FROM public_holidays a
 USING public_holidays b
 WHERE a.holiday_date = b.holiday_date
   AND a.source = b.source
   AND a.created_at > b.created_at;

ALTER TABLE public_holidays
    ADD CONSTRAINT public_holidays_holiday_date_source_key UNIQUE (holiday_date, source);

ALTER TABLE public_holidays DROP CONSTRAINT IF EXISTS public_holidays_window_check;

ALTER TABLE public_holidays
    DROP COLUMN start_time,
    DROP COLUMN end_time;
