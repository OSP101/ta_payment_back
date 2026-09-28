-- Staff reminders to TAs who have not entered their class timetable, sent from
-- the ใบแต่งตั้ง TA page. A pending TA request cannot be decided until every
-- TA on it has a timetable, so these TAs hold up the appointment order.
-- One row per reminder: the latest one drives the "เตือนล่าสุด" label and the
-- once-a-day limit per TA and term.
CREATE TABLE IF NOT EXISTS timetable_reminders (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ta_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    term_id  UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    sent_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_by  UUID REFERENCES users(id)
);
CREATE INDEX IF NOT EXISTS timetable_reminders_ta_term_idx
    ON timetable_reminders (ta_id, term_id, sent_at DESC);
