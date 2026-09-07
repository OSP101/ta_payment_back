ALTER TABLE makeup_schedules
    DROP CONSTRAINT makeup_schedules_waived_shape;

DELETE FROM makeup_schedules WHERE waived;

ALTER TABLE makeup_schedules
    DROP COLUMN waived,
    DROP COLUMN waived_by,
    DROP COLUMN waived_at,
    ALTER COLUMN makeup_date SET NOT NULL;
