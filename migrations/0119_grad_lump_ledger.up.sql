-- The graduate-special lump each exported month was paid, frozen at export.
--
-- The lump is a flat per-term figure split over the term's months by weight
-- (gradLumpByMonth), and the split used to be recomputed live on every read:
-- a later month gaining weight shrank a month finance had already been sent,
-- and the next claim document carried the new, smaller figure for it. A row
-- here is what the document said for that month; the split only redistributes
-- what is left over the months that are not frozen yet.
--
-- lump_basis is the whole-term lump the first freeze used. It is the same on
-- every row of one (course, TA) and fixes the lump for the rest of the term, so
-- a rate change mid-term cannot leave the frozen months totalling more than a
-- new, smaller lump (decision 23/09/2026: lock the lump at the first freeze).
--
-- Append-only: nothing un-exports a month's money once finance has it.
CREATE TABLE grad_lump_ledger (
    teaching_course_id UUID          NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    ta_id              UUID          NOT NULL REFERENCES users(id),
    year_month         TEXT          NOT NULL CHECK (year_month ~ '^[0-9]{4}-[0-9]{2}$'),
    baht               NUMERIC(12,2) NOT NULL CHECK (baht >= 0),
    lump_basis         NUMERIC(12,2) NOT NULL CHECK (lump_basis >= 0),
    frozen_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    frozen_by          UUID          REFERENCES users(id),
    PRIMARY KEY (teaching_course_id, ta_id, year_month)
);
