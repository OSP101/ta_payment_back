ALTER TABLE ta_requests
  DROP COLUMN IF EXISTS lecturer_note,
  DROP COLUMN IF EXISTS lecturer_notified_at;
