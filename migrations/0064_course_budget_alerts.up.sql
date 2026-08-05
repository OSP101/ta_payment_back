-- Remembers the last budget shortfall a course was warned about.
--
-- Without it there is no way to send the warning exactly once. The shortfall is
-- recomputed on every approval, so a stateless "notify if over budget" would
-- fire again on each one; and the coalescing in NotifyService only suppresses
-- repeats while the notice is still UNREAD, so the day after somebody reads it
-- the noise starts again.
--
-- What is stored is the CUTOFF MONTH, not a boolean: re-notifying is right when
-- the answer changes ("now October too", or "it fits again after staff raised
-- the student count") and wrong when it merely gets recomputed to the same
-- thing. Comparing the month makes that distinction for free.
--
-- last_cutoff NULL means the course is currently within budget — kept as a row
-- rather than deleted so the transition back into shortfall is still a change.
CREATE TABLE course_budget_alerts (
    teaching_course_id UUID PRIMARY KEY REFERENCES teaching_courses(id) ON DELETE CASCADE,
    last_cutoff        TEXT,
    notified_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
