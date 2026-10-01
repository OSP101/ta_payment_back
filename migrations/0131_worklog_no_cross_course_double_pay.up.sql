-- One TA, one minute, one payment.
--
-- Billing (billable_hours.go) merges a TA's sittings per teaching course, so two
-- rows in the SAME course that overlap are paid once. Two courses are billed
-- separately and nothing merged them: when two approved courses had lectures at
-- the same hour (allowed at request time on purpose) Generate wrote 09:00–12:00
-- in both, and both were approved and paid — 15 h of work billed as 30.
--
-- The service now refuses this at Generate, Submit and Approve. This trigger is
-- the same rule at the row, for every path that reaches 'approved' (including
-- ones written later): an approved row may not overlap another approved row of
-- the same TA in a DIFFERENT course. Same-course overlap is left to the sitting
-- merge and the co-taught rule (B2), which pay it once by design.
--
-- Only fires when the NEW row is approved, so existing data is untouched until
-- someone edits it, and DELETE is never blocked.
CREATE OR REPLACE FUNCTION refuse_cross_course_double_pay() RETURNS trigger AS $$
DECLARE
    other_code TEXT;
    other_start TEXT;
    other_end TEXT;
BEGIN
    IF NEW.status <> 'approved' THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.status = 'approved'
       AND OLD.work_date = NEW.work_date
       AND OLD.start_time = NEW.start_time AND OLD.end_time = NEW.end_time
       AND OLD.assignment_id = NEW.assignment_id THEN
        RETURN NEW;
    END IF;
    SELECT otc.code, to_char(o.start_time, 'HH24:MI'), to_char(o.end_time, 'HH24:MI')
      INTO other_code, other_start, other_end
      FROM ta_request_assignments na
      JOIN sections nsec ON nsec.id = na.section_id
      JOIN ta_request_assignments oa ON oa.ta_id = na.ta_id
      JOIN sections osec ON osec.id = oa.section_id
                        AND osec.teaching_course_id <> nsec.teaching_course_id
      JOIN teaching_courses otc ON otc.id = osec.teaching_course_id
      JOIN work_logs o ON o.assignment_id = oa.id
                      AND o.status = 'approved'
                      AND o.id <> NEW.id
                      AND o.work_date = NEW.work_date
                      AND o.start_time < NEW.end_time
                      AND o.end_time > NEW.start_time
     WHERE na.id = NEW.assignment_id
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'วันที่ % เวลา %–% ซ้อนกับชั่วโมงที่อนุมัติแล้วของรายวิชา % (%–%) ผู้ช่วยสอนรับค่าตอบแทนช่วงเวลาเดียวกันซ้ำไม่ได้',
            to_char(NEW.work_date, 'FMDD/FMMM/') || (EXTRACT(YEAR FROM NEW.work_date)::int + 543), to_char(NEW.start_time, 'HH24:MI'), to_char(NEW.end_time, 'HH24:MI'),
            other_code, other_start, other_end
            USING ERRCODE = 'P0001';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS work_logs_refuse_cross_course_double_pay ON work_logs;
CREATE TRIGGER work_logs_refuse_cross_course_double_pay
    BEFORE INSERT OR UPDATE ON work_logs
    FOR EACH ROW EXECUTE FUNCTION refuse_cross_course_double_pay();
