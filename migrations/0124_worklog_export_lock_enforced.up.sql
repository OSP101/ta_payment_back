-- The export/finance month lock was enforced only in Go (assertWorklogWritable),
-- which reads the status before the write and outside any transaction. A write
-- that passed that check just before a ZIP export committed its lock still
-- landed in the now-exported month, and the file finance holds no longer
-- matched the database. This trigger is the same rule at the row itself, so it
-- holds whatever the timing. DELETE is left alone: the app only deletes draft
-- or rejected rows (which an exported month cannot hold), and cascades from a
-- term, course or PDPA erasure must not be blocked by it.
CREATE OR REPLACE FUNCTION refuse_worklog_write_in_locked_month() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND ROW(OLD.*) IS NOT DISTINCT FROM ROW(NEW.*) THEN
        RETURN NEW;
    END IF;
    IF EXISTS (
        SELECT 1
          FROM (SELECT NEW.assignment_id AS assignment_id, NEW.work_date AS work_date
                UNION
                SELECT OLD.assignment_id, OLD.work_date WHERE TG_OP = 'UPDATE') w
          JOIN ta_request_assignments a ON a.id = w.assignment_id
          JOIN sections sec             ON sec.id = a.section_id
          JOIN teaching_courses tc      ON tc.id = sec.teaching_course_id
          JOIN academic_terms trm       ON trm.id = tc.term_id
          JOIN submission_periods sp    ON sp.term_id = tc.term_id
           AND sp.year_month = trm.academic_year::text || '-' || to_char(w.work_date, 'MM')
          JOIN submission_period_status st
            ON st.submission_period_id = sp.id
           AND st.ta_id = a.ta_id
           AND st.teaching_course_id = tc.id
         WHERE st.status IN ('exported', 'finance_sent')
    ) THEN
        RAISE EXCEPTION 'บันทึกเวลาเดือนนี้ถูกส่งออกไฟล์เบิกจ่ายแล้ว แก้ไขไม่ได้'
            USING ERRCODE = 'P0001';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS work_logs_refuse_locked_month ON work_logs;
CREATE TRIGGER work_logs_refuse_locked_month
    BEFORE INSERT OR UPDATE ON work_logs
    FOR EACH ROW EXECUTE FUNCTION refuse_worklog_write_in_locked_month();
