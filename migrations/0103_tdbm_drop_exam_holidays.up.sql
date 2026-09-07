-- TDBM's h_type field was undocumented (see docs/TDBM-API-requirements.md
-- §3.2) when SyncHolidays first shipped, so every row it returned — including
-- exam days (h_type='D') — was imported into public_holidays alongside real
-- holidays (h_type='E'). Confirmed with the college that D = exam day, not a
-- holiday: exam days must never block worklog entry or count toward the
-- lecturer "unresolved holiday" reminder, and staff already enter real
-- holidays into this table by hand. Remove the rows this bug already wrote.
--
-- Scoped tightly to avoid touching anything a human entered: source='tdbm'
-- (never a staff-entered row) AND the note this sync writes literally tags
-- h_type=D (see tdbm.go's SyncHolidays note format).
DELETE FROM public_holidays
WHERE source = 'tdbm'
  AND note LIKE '%h_type=D%';
