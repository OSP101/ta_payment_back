ALTER TABLE ta_review_schedules DROP CONSTRAINT ta_review_schedules_kind_check;
ALTER TABLE ta_review_schedules ADD CONSTRAINT ta_review_schedules_kind_check CHECK (kind IN ('review','other_lecture','other_lab','prep','grad_other'));
