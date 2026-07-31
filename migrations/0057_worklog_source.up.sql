-- Record who created each work-log row: the generator, or the TA by hand.
--
-- WHY IT MATTERS. On the payout-review screen a staff member has to decide where
-- to spend attention. A row the generator produced is a copy of the section's
-- timetable — the lecturer entered those class times, so there is nothing in it
-- to verify. A row the TA typed is a claim derived from nothing. Those two
-- deserve very different amounts of looking at, and until now the screen could
-- not tell them apart.
--
-- It could not tell because nothing recorded it. The generator writes a Thai
-- label into `note` ("เช็คชื่อ", "สอนปฏิบัติ", …) and the TA is free to edit that
-- note, so origin was never stored anywhere — only hinted at, in a field the
-- subject of the audit controls.

ALTER TABLE work_logs
    ADD COLUMN source TEXT NOT NULL DEFAULT 'manual'
        CHECK (source IN ('auto', 'manual'));

COMMENT ON COLUMN work_logs.source IS
    'auto = created by WorkLogService.Generate from the section timetable; '
    'manual = entered by the TA. Set at INSERT and never rewritten — editing a '
    'generated row does not make it hand-made, and re-labelling it would erase '
    'the fact that a machine proposed those hours.';

-- The DEFAULT is 'manual' on purpose: every existing row is unlabelled, and the
-- backfill below can only guess. Guessing "manual" costs a staff member a glance
-- at a row that was fine; guessing "auto" hides a hand-typed row from review.
-- Only one of those two errors is acceptable, so the default takes the safe side
-- and the backfill only promotes a row to 'auto' on strong evidence.

-- Backfill: promote to 'auto' only when TWO independent signals agree —
--
--   1. the row lands exactly on one of its section's scheduled periods
--      (same weekday, same start and end time, same kind), which is the only
--      shape the generator produces; and
--   2. its note is still verbatim what autoNoteFor() writes, meaning nobody has
--      edited the row's description since.
--
-- Either signal alone is too weak. A TA logging a normal class by hand matches
-- (1) exactly; a TA can type the same note as (2) by coincidence. Requiring both
-- keeps the false-'auto' rate near zero, which is the direction that matters.
--
-- The note strings are duplicated from autoNoteFor in internal/service/worklog.go.
-- They are a historical fact here — the labels as they were when these rows were
-- written — so they must NOT be kept in sync if that function later changes.
UPDATE work_logs wl
   SET source = 'auto'
  FROM ta_request_assignments a
 WHERE a.id = wl.assignment_id
   AND wl.note IN ('เช็คชื่อ', 'สอนปฏิบัติ', 'ตรวจงาน', 'ชดเชย', 'งานอื่นๆ',
                   'เช็คชื่อ(ชดเชย)', 'สอนปฏิบัติ(ชดเชย)', 'ตรวจงาน(ชดเชย)',
                   'ชดเชย(ชดเชย)', 'งานอื่นๆ(ชดเชย)')
   AND (
       -- Class periods come from the section timetable.
       EXISTS (
           SELECT 1
             FROM section_schedules sch
            WHERE sch.section_id = a.section_id
              AND sch.kind = wl.activity
              AND sch.start_time = wl.start_time
              AND sch.end_time = wl.end_time
              AND sch.day_of_week = EXTRACT(DOW FROM wl.work_date)::int
       )
       -- ...but a row moved onto a makeup date lands on a different weekday, so
       -- match those against the makeup instead of the weekly slot.
       OR EXISTS (
           SELECT 1
             FROM makeup_schedules m
             JOIN section_schedules sch
               ON sch.section_id = m.section_id
              AND sch.kind = m.kind
            WHERE m.section_id = a.section_id
              AND m.makeup_date = wl.work_date
              AND sch.kind = wl.activity
       )
       -- Grading rows are generated from the TA's own review timetable, which
       -- lives in a different table entirely. Leaving this out labelled every
       -- generated grading row 'manual' — on the real database that was the
       -- whole manual bucket, which would have made the distinction useless.
       OR EXISTS (
           SELECT 1
             FROM ta_review_schedules rs
            WHERE rs.assignment_id = wl.assignment_id
              AND wl.activity = 'review'
              AND rs.start_time = wl.start_time
              AND rs.end_time = wl.end_time
              AND rs.day_of_week = EXTRACT(DOW FROM wl.work_date)::int
       )
   );

-- The review screen groups a month's rows by source, so this is the access path.
CREATE INDEX work_logs_assignment_source_idx
    ON work_logs (assignment_id, source);
