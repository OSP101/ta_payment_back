-- What a TA's assignment looked like before their own timetable cut it
-- (state + the in-class duty hours stripBlockedInClassHours zeroes). Without
-- it, a TA who typed a class by mistake, got trimmed/dropped, and then deleted
-- the class stayed cut forever: the recheck never restored anything and the
-- lecturer's declared hours were gone. NULL = never cut by a clash.
ALTER TABLE ta_request_assignments ADD COLUMN IF NOT EXISTS pre_clash JSONB;
