DROP TABLE IF EXISTS tdbm_sync_log;
DROP TABLE IF EXISTS tdbm_extra_teachings;
DROP TABLE IF EXISTS tdbm_teachers;

DO $$
DECLARE
    con_name text;
BEGIN
    SELECT conname INTO con_name
    FROM pg_constraint
    WHERE conrelid = 'public_holidays'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) ILIKE '%source%';
    IF con_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE public_holidays DROP CONSTRAINT %I', con_name);
    END IF;
END $$;

ALTER TABLE public_holidays
    ADD CONSTRAINT public_holidays_source_check
    CHECK (source IN ('national', 'university', 'faculty', 'custom'));

ALTER TABLE public_holidays
    DROP COLUMN IF EXISTS tdbm_holiday_id;
