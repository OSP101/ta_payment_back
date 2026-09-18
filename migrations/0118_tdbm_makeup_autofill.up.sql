-- Tracks which tdbm_extra_teachings row was used to auto-create which
-- makeup_schedules row — see TDBMService.AutoFillMakeupSchedules.
--
-- Needed for correctness, not just bookkeeping: auto-fill re-runs on every
-- sync and every course/section change, pairing a section's still-unresolved
-- holiday periods against its still-unconsumed TDBM entries in date order
-- (TDBM gives no explicit link between a compensation submission and the
-- holiday it replaces — see migration 0116's header comment). Without
-- excluding already-applied entries from that candidate list, a re-run could
-- re-pair an entry that already filled one holiday against a DIFFERENT
-- holiday that opened up later, inserting a wrong (and by then harmless-
-- looking, since it is a fresh unique key) makeup row pointing at the wrong
-- original date.
ALTER TABLE tdbm_extra_teachings
    ADD COLUMN applied_makeup_id UUID REFERENCES makeup_schedules(id);

CREATE INDEX idx_tdbm_extra_teachings_applied ON tdbm_extra_teachings (applied_makeup_id);
