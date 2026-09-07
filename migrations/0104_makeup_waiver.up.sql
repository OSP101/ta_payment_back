-- "No makeup needed" — a lecturer/staff/TA declaration that a cancelled
-- period will deliberately not get a makeup, distinct from "not filed yet".
-- Modeled as a row in makeup_schedules with makeup_date left NULL, occupying
-- the same (section_id, original_date, kind) slot a real makeup would use
-- (migration 0055) — so ImpactsForCourse's LEFT JOIN treats it as resolved
-- exactly like a filed makeup, without needing a second table or query.
ALTER TABLE makeup_schedules
    ALTER COLUMN makeup_date DROP NOT NULL,
    ADD COLUMN waived BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN waived_by UUID REFERENCES users(id),
    ADD COLUMN waived_at TIMESTAMPTZ;

ALTER TABLE makeup_schedules
    ADD CONSTRAINT makeup_schedules_waived_shape CHECK (
        (waived AND makeup_date IS NULL AND start_time IS NULL AND end_time IS NULL)
        OR (NOT waived AND makeup_date IS NOT NULL)
    );

COMMENT ON COLUMN makeup_schedules.waived IS
    'true = a manager/TA confirmed this cancelled period will not be made up. '
    'A waived row has no makeup_date/start_time/end_time and is treated as '
    'resolved everywhere a filed makeup would be.';
