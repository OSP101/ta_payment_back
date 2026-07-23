-- Add a "faculty" (คณะ) holiday type alongside national/university/custom so
-- staff can record college-level closures distinctly. The original CHECK on
-- public_holidays.source was created inline (system-generated name); find and
-- drop it, then re-add the widened constraint.
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
