-- One submission used to be one ta_requests row holding every TA, judged as a
-- unit: a single TA failing a rule rejected the others with them, and a single
-- TA without a timetable held everyone back (03/10/2026: "ใครผ่านก็คนนั้นผ่าน
-- เลย ไม่ต้องรอคนอื่น"). A submission is now filed as one request PER TA, so
-- every existing per-request rule judges that TA alone. batch_id ties the
-- rows of one submission back together; rows from before this have none.
ALTER TABLE ta_requests ADD COLUMN batch_id UUID;
CREATE INDEX ta_requests_batch_id_idx ON ta_requests (batch_id) WHERE batch_id IS NOT NULL;
