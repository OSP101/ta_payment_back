-- A makeup belongs to one PERIOD, not to a whole section-day.
--
-- THE BUG. uq_makeup_section_original made the row unique per (section,
-- original_date), so a section could hold exactly one makeup for a cancelled day.
-- But a section commonly holds two periods on the same weekday — SC363001 sec 1
-- has บรรยาย 10:30–12:30 and ปฏิบัติการ 13:00–15:00 both on Tuesday. Filing a
-- makeup for the lab therefore:
--
--   * marked the lecture as made up as well, because every reader joined the
--     makeup on (section, date) and matched both periods; and
--   * showed the lab's replacement time on the lecture row, asserting that a
--     2-hour lecture and a 2-hour lab would run simultaneously in one slot.
--
-- The lecturer could not fix it either: the unique constraint refused a second
-- makeup for that day, so the lecture's real replacement slot could not be filed
-- at all.
--
-- WHY `kind` AND NOT `section_schedule_id`. A schedule row is the more precise
-- target, but section_schedules is DELETEd and re-INSERTed wholesale whenever a
-- schedule is set or re-imported (TeachingService.SetSectionSchedule), so an FK
-- to it — cascading or not — would destroy or orphan filed makeups on every
-- registrar import. `kind` is a stable label that survives re-import, and
-- (section, weekday, kind) is what the UI groups by anyway.

ALTER TABLE makeup_schedules
    ADD COLUMN kind TEXT;

COMMENT ON COLUMN makeup_schedules.kind IS
    'Which period of that day this makeup replaces: lecture | lab. A section-day '
    'with both holds two independent makeup rows.';

-- The old constraint has to go BEFORE the backfill, not after: expanding one
-- section-day row into one row per period creates a second row with the same
-- (section_id, original_date), which is exactly what it forbids. Dropping it
-- afterwards was an ordering bug that passed on an empty test database — where
-- there was nothing to expand — and failed on the first real row.
ALTER TABLE makeup_schedules
    DROP CONSTRAINT uq_makeup_section_original;

-- Backfill. Every existing row was section-wide by construction, so preserving
-- its meaning means expanding it into one row per period that the section
-- actually teaches on that weekday: what the lecturer sees stays exactly the
-- same, and each period becomes separately editable.
--
-- The FIRST kind (alphabetically, so the result is deterministic) keeps the
-- original row and its id — that id may appear in an audit entry, and rewriting
-- history to a new uuid would make those entries unresolvable.
WITH day_kinds AS (
    SELECT m.id AS makeup_id, sch.kind,
           ROW_NUMBER() OVER (PARTITION BY m.id ORDER BY sch.kind) AS rn
      FROM makeup_schedules m
      JOIN section_schedules sch
        ON sch.section_id = m.section_id
       AND sch.day_of_week = EXTRACT(DOW FROM m.original_date)::int
     GROUP BY m.id, sch.kind
)
UPDATE makeup_schedules m
   SET kind = dk.kind
  FROM day_kinds dk
 WHERE dk.makeup_id = m.id AND dk.rn = 1;

INSERT INTO makeup_schedules (id, section_id, original_date, makeup_date, start_time, end_time, note, kind)
SELECT gen_random_uuid(), m.section_id, m.original_date, m.makeup_date,
       m.start_time, m.end_time, m.note, dk.kind
  FROM makeup_schedules m
  JOIN (
    SELECT m2.id AS makeup_id, sch.kind,
           ROW_NUMBER() OVER (PARTITION BY m2.id ORDER BY sch.kind) AS rn
      FROM makeup_schedules m2
      JOIN section_schedules sch
        ON sch.section_id = m2.section_id
       AND sch.day_of_week = EXTRACT(DOW FROM m2.original_date)::int
     GROUP BY m2.id, sch.kind
  ) dk ON dk.makeup_id = m.id AND dk.rn > 1;

-- A makeup whose section has no schedule on that weekday matched no kind above
-- and is still NULL. That is a row nothing could ever have matched — the period
-- it replaces does not exist — but deleting a lecturer's decision during a
-- migration is worse than keeping it, so it is parked on 'lecture' and left
-- visible. NOT NULL below would otherwise fail the whole migration on it.
UPDATE makeup_schedules SET kind = 'lecture' WHERE kind IS NULL;

ALTER TABLE makeup_schedules
    ALTER COLUMN kind SET NOT NULL,
    ADD CONSTRAINT makeup_schedules_kind_check CHECK (kind IN ('lecture', 'lab'));

-- One makeup per period, not per day. This is the constraint whose old form was
-- the bug.
ALTER TABLE makeup_schedules
    ADD CONSTRAINT uq_makeup_section_original_kind
        UNIQUE (section_id, original_date, kind);
