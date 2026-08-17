ALTER TABLE ta_request_assignments
    DROP COLUMN IF EXISTS enrollment_id,
    DROP COLUMN IF EXISTS student_id_snapshot;
