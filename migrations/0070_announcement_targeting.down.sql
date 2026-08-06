DROP INDEX IF EXISTS announcement_recipients_user_idx;
ALTER TABLE announcements
    DROP COLUMN IF EXISTS target_course_ids,
    DROP COLUMN IF EXISTS target_user_ids,
    DROP COLUMN IF EXISTS target_filters,
    DROP COLUMN IF EXISTS target_term_id;
