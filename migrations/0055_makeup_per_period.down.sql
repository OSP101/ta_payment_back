-- Collapsing back to one makeup per section-day is lossy: where a lecturer filed
-- different replacement slots for the lecture and the lab, only one can survive.
--
-- The earliest makeup_date is kept (then the earliest start_time), so the row that
-- remains is the one a TA would reach first. The rest are deleted — reverting this
-- migration means going back to a model that cannot express them.
DELETE FROM makeup_schedules m
 WHERE EXISTS (
   SELECT 1 FROM makeup_schedules keep
    WHERE keep.section_id = m.section_id
      AND keep.original_date = m.original_date
      AND keep.id <> m.id
      AND (keep.makeup_date, COALESCE(keep.start_time, '00:00'::time), keep.id)
        < (m.makeup_date,    COALESCE(m.start_time,    '00:00'::time), m.id)
 );

ALTER TABLE makeup_schedules
    DROP CONSTRAINT uq_makeup_section_original_kind,
    ADD CONSTRAINT uq_makeup_section_original UNIQUE (section_id, original_date);

ALTER TABLE makeup_schedules
    DROP CONSTRAINT makeup_schedules_kind_check,
    DROP COLUMN kind;
