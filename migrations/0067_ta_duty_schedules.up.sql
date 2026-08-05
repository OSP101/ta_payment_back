-- Two related gaps in how a TA's declared duties reach a timetable.
--
-- 1. ปฏิบัติการ had exactly one field — "จำนวนชั่วโมง" (lab_hrs), meaning "run
--    the lab". There was no way to say a TA supports a lab section WITHOUT
--    standing in the lab: prep, materials, marking lab sheets. Those are real
--    and they happen outside the slot. lab_other_hrs is that alternative, and
--    the two are mutually exclusive (enforced in validateUndergradSectionCaps,
--    not here — a CHECK would also reject the all-zero row every request starts
--    from).
--
-- 2. ta_review_schedules held the TA's weekly grading slots. "อื่น ๆ" had no
--    equivalent, so a lecturer could declare other-work hours and the TA had
--    nowhere to say when they would do them — auto-generate then produced
--    nothing, silently. Rather than a second near-identical table, the existing
--    one grows a kind: one card component, one generator loop, one conflict
--    check covering all three duties.
ALTER TABLE ta_workload_forms
    ADD COLUMN lab_other_hrs  NUMERIC(4,2) DEFAULT 0,
    ADD COLUMN lab_other_desc TEXT;

-- Every existing row is a grading slot, which is exactly what the default says.
ALTER TABLE ta_review_schedules
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'review';

ALTER TABLE ta_review_schedules
    ADD CONSTRAINT ta_review_schedules_kind_check
    CHECK (kind IN ('review', 'other_lecture', 'other_lab'));

-- The old uniqueness spans (assignment, day, start, end) with no kind, so a TA
-- could not put grading and other-work in the same slot on different weeks'
-- worth of planning — and more importantly the two kinds would collide as
-- duplicates. Widen it rather than drop it: two slots of the SAME kind at the
-- same time is still a mistake worth refusing.
ALTER TABLE ta_review_schedules
    DROP CONSTRAINT IF EXISTS ta_review_schedules_assignment_id_day_of_week_start_time_en_key;

ALTER TABLE ta_review_schedules
    ADD CONSTRAINT ta_review_schedules_slot_key
    UNIQUE (assignment_id, kind, day_of_week, start_time, end_time);
