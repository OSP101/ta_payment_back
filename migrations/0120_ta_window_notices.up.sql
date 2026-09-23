-- Lecturer e-mail notices for a TA request window.
--
-- notify_lecturers: whether the scheduler mails lecturers about this window.
-- Existing windows default to FALSE so deploying this does not mail every
-- lecturer about a window that opened weeks ago; the API defaults a NEW window
-- to TRUE (see TARequestService.UpsertWindow).
ALTER TABLE ta_request_windows
    ADD COLUMN IF NOT EXISTS notify_lecturers BOOLEAN NOT NULL DEFAULT FALSE;

-- One row per (window, lecturer, kind) that has been mailed. The primary key is
-- the whole de-duplication guarantee: the sweep claims a row BEFORE sending, so
-- editing the window, the hourly tick and the immediate post-save sweep can
-- never mail the same lecturer twice for the same thing.
--   open    — "requests are open", sent once the lecturer has >= 1 course
--   closing — "deadline in a few days", only for courses with no request yet
CREATE TABLE IF NOT EXISTS ta_window_notices (
    window_id   UUID NOT NULL REFERENCES ta_request_windows(id) ON DELETE CASCADE,
    lecturer_id UUID NOT NULL REFERENCES users(id),
    kind        TEXT NOT NULL CHECK (kind IN ('open', 'closing')),
    sent_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (window_id, lecturer_id, kind)
);
