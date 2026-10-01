ALTER TABLE ta_class_schedules
    DROP COLUMN IF EXISTS added_after_approval,
    DROP COLUMN IF EXISTS created_at;
