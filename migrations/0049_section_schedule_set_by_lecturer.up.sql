-- Record when a lecturer (as opposed to staff or the registrar import) filled
-- in a section's timetable.
--
-- Rule the column serves: a lecturer may set the timetable of a section that
-- has none — the "WBA" case, where the registrar file carried no meeting times
-- — and may do so exactly once. Everything after that is staff's to change.
--
-- Enforcement itself only needs "does this section have schedule rows", which
-- is already derivable. This column exists so the refusal can say WHICH rule
-- the lecturer hit:
--
--   rows exist, stamp NULL      → the timetable came from the imported file
--   rows exist, stamp NOT NULL  → you already used your one edit, on <date>
--   stamp NOT NULL, no rows     → staff cleared it; still not yours to refill
--
-- NULL on every existing row is correct, not a backfill gap: nothing before
-- this migration was written under the one-shot rule, so no lecturer has
-- spent their edit yet.

ALTER TABLE sections
    ADD COLUMN schedule_set_by_lecturer_at TIMESTAMPTZ;
