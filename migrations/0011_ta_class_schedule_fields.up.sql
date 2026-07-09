-- Split the free-form course_label on ta_class_schedules into structured
-- fields so the TA can record course code, name, section, and whether the
-- block is a lecture or a lab. Overlap checks against teaching sections
-- keep working on (day_of_week, start_time, end_time) — the new columns
-- are display-only.
ALTER TABLE ta_class_schedules
    ADD COLUMN IF NOT EXISTS course_code TEXT,
    ADD COLUMN IF NOT EXISTS course_name TEXT,
    ADD COLUMN IF NOT EXISTS kind        TEXT,
    ADD COLUMN IF NOT EXISTS sec_no      TEXT;

-- Kind is optional; when present it must be lecture or lab.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'ta_class_schedules_kind_check'
    ) THEN
        ALTER TABLE ta_class_schedules
            ADD CONSTRAINT ta_class_schedules_kind_check
            CHECK (kind IS NULL OR kind IN ('lecture','lab'));
    END IF;
END$$;

-- Backfill: whatever the TA typed in the old free-form course_label
-- becomes the new course_name so previously-saved schedules keep their
-- label. Empty labels stay empty.
UPDATE ta_class_schedules
   SET course_name = course_label
 WHERE course_name IS NULL
   AND course_label IS NOT NULL
   AND course_label <> '';
