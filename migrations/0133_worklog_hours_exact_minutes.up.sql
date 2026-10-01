-- Hours and money must come from ONE quantity: the minutes between start and end.
--
-- work_logs.hours was NUMERIC(4,2), so a 23:00–23:59 row stored 0.98 while the
-- claim form and the billing sittings (billable_hours.go, claimHours) priced the
-- 59 minutes themselves: 0.98333… × 40 = 39.33 ฿ on the form, 0.98 × 40 = 39.20 ฿
-- on every screen that multiplies stored hours by the rate. A month of such rows
-- showed 13.98 h × 40 = 559.20 next to a payout of 559.33.
--
-- Decision (01/10/2026): the printed form is what the college pays, and it is
-- minute-based, so MINUTES are the quantity. hours becomes an exact derivative
-- of the clock range — NUMERIC(10,6), so the stored value differs from minutes/60
-- by under 0.000001 h (well below a satang over a whole term) — and screens round
-- only when they DISPLAY it.
ALTER TABLE work_logs ALTER COLUMN hours TYPE NUMERIC(10,6);

-- Every writer (TA save, staff/lecturer edit, generate, demo seeding) is made
-- consistent at the row instead of in five Go call sites. Only a value that is
-- already the span to within rounding is snapped; a row whose hours genuinely
-- differ from its clock range is left exactly as written, so this can never
-- change what someone meant to claim.
CREATE OR REPLACE FUNCTION work_log_hours_from_times() RETURNS trigger AS $$
DECLARE
    span NUMERIC;
BEGIN
    IF TG_OP = 'UPDATE'
       AND OLD.start_time IS NOT DISTINCT FROM NEW.start_time
       AND OLD.end_time   IS NOT DISTINCT FROM NEW.end_time
       AND OLD.hours      IS NOT DISTINCT FROM NEW.hours THEN
        RETURN NEW;
    END IF;
    IF NEW.start_time IS NULL OR NEW.end_time IS NULL OR NEW.end_time <= NEW.start_time THEN
        RETURN NEW;
    END IF;
    span := EXTRACT(EPOCH FROM (NEW.end_time - NEW.start_time)) / 3600.0;
    IF abs(NEW.hours - span) <= 0.011 THEN
        NEW.hours := round(span, 6);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS work_logs_hours_from_times ON work_logs;
CREATE TRIGGER work_logs_hours_from_times
    BEFORE INSERT OR UPDATE ON work_logs
    FOR EACH ROW EXECUTE FUNCTION work_log_hours_from_times();

-- Existing rows: snap the rounding-only mismatches. User triggers are off for
-- this one statement because it is not an edit anybody made — it must not reset
-- a staff sign-off (0123) or be refused in an exported month (0124): the file
-- already sent to finance was priced from the minutes, so this makes the
-- database agree with it, not the other way round.
ALTER TABLE work_logs DISABLE TRIGGER USER;
UPDATE work_logs
   SET hours = round(EXTRACT(EPOCH FROM (end_time - start_time)) / 3600.0, 6)
 WHERE end_time > start_time
   AND hours <> round(EXTRACT(EPOCH FROM (end_time - start_time)) / 3600.0, 6)
   AND abs(hours - EXTRACT(EPOCH FROM (end_time - start_time)) / 3600.0) <= 0.011;
ALTER TABLE work_logs ENABLE TRIGGER USER;
