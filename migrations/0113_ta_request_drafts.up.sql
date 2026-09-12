-- Unsent TA-request forms, saved as the lecturer types so a refresh, a
-- session timeout or a different machine brings back what they had. One draft
-- per lecturer per course; the payload is the form state verbatim (the
-- frontend owns its shape) and is thrown away on a successful submit.
CREATE TABLE IF NOT EXISTS ta_request_drafts (
    user_id            UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    teaching_course_id UUID NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    payload            JSONB NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, teaching_course_id)
);
