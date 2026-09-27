-- A staff sign-off (staff_reviewed) certifies the hours of one TA's month on
-- one course, and MarkCourseExported bills whatever is approved when the file
-- is built. Several paths can still change those hours after the sign-off: a
-- TA moving a rejected row into the month, StaffUpsert creating a row, a
-- lecturer editing an approved row behind a password step-up. None of them
-- touched the status, so the export billed hours staff never checked.
--
-- Rather than chase every write path, any change to a work_log row sends a
-- signed-off month back to 'pending' (the same place a staff send-back puts
-- it), naming the system as the sender. Exported/finance_sent months are
-- untouched: writes there are already refused by assertWorklogWritable.
CREATE OR REPLACE FUNCTION reset_staff_review_on_worklog_change() RETURNS trigger AS $$
DECLARE
    r RECORD;
BEGIN
    IF TG_OP = 'UPDATE' AND ROW(OLD.*) IS NOT DISTINCT FROM ROW(NEW.*) THEN
        RETURN NULL;
    END IF;
    FOR r IN
        SELECT a.ta_id, sec.teaching_course_id AS tc_id, sp.id AS period_id
          FROM (SELECT NEW.assignment_id AS assignment_id, NEW.work_date AS work_date
                 WHERE TG_OP <> 'DELETE'
                UNION
                SELECT OLD.assignment_id, OLD.work_date
                 WHERE TG_OP <> 'INSERT') w
          JOIN ta_request_assignments a ON a.id = w.assignment_id
          JOIN sections sec             ON sec.id = a.section_id
          JOIN teaching_courses tc      ON tc.id = sec.teaching_course_id
          JOIN academic_terms trm       ON trm.id = tc.term_id
          JOIN submission_periods sp    ON sp.term_id = tc.term_id
           AND sp.year_month = trm.academic_year::text || '-' || to_char(w.work_date, 'MM')
    LOOP
        UPDATE submission_period_status SET
            status              = 'pending',
            staff_reviewed_by   = NULL,
            staff_reviewed_name = NULL,
            sent_back_at        = now(),
            sent_back_by        = NULL,
            sent_back_name      = 'ระบบ',
            sent_back_reason    = 'บันทึกเวลาของเดือนนี้เปลี่ยนหลังเจ้าหน้าที่ตรวจแล้ว กรุณาตรวจอีกครั้ง'
         WHERE submission_period_id = r.period_id
           AND ta_id = r.ta_id
           AND teaching_course_id = r.tc_id
           AND status = 'staff_reviewed';
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS work_logs_reset_staff_review ON work_logs;
CREATE TRIGGER work_logs_reset_staff_review
    AFTER INSERT OR UPDATE OR DELETE ON work_logs
    FOR EACH ROW EXECUTE FUNCTION reset_staff_review_on_worklog_change();
