ALTER TABLE makeup_schedules DROP CONSTRAINT IF EXISTS uq_makeup_section_original;
DROP INDEX IF EXISTS ix_hrl_recent;
DROP TABLE IF EXISTS holiday_remind_log;
DROP INDEX IF EXISTS ix_public_holidays_date;
DROP TABLE IF EXISTS public_holidays;
