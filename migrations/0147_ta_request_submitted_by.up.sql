-- Staff may file a TA request on a lecturer's behalf (03/10/2026): lecturer_id
-- stays the course lecturer the request is for, submitted_by records the
-- officer who actually pressed send. NULL = the lecturer sent it themselves.
ALTER TABLE ta_requests
    ADD COLUMN submitted_by UUID REFERENCES users(id) ON DELETE SET NULL;
