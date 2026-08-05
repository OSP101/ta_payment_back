-- Slots for the kinds that did not exist before are deleted, not converted:
-- calling other-work "grading" would put hours on the wrong line of the claim
-- book, and there is no honest way to map them back.
DELETE FROM ta_review_schedules WHERE kind <> 'review';

ALTER TABLE ta_review_schedules
    DROP CONSTRAINT IF EXISTS ta_review_schedules_slot_key;

ALTER TABLE ta_review_schedules
    DROP CONSTRAINT IF EXISTS ta_review_schedules_kind_check;

ALTER TABLE ta_review_schedules DROP COLUMN kind;

ALTER TABLE ta_review_schedules
    ADD CONSTRAINT ta_review_schedules_assignment_id_day_of_week_start_time_en_key
    UNIQUE (assignment_id, day_of_week, start_time, end_time);

ALTER TABLE ta_workload_forms
    DROP COLUMN lab_other_hrs,
    DROP COLUMN lab_other_desc;
