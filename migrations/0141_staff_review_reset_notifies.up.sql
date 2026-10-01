-- 0123 sends a signed-off (staff_reviewed) month back to 'pending' when any of
-- its work_log rows changes. Two things were missing.
--
-- 1. Nobody was told. The month silently dropped out of "ready to export" and
--    the officer who had signed it off found out only when the export bar
--    refused, with no idea which row moved or why. Every active staff account
--    now gets an in-app notice. Its TITLE names the course, the TA and the
--    month: unread in-app notices are folded by (title, link) (see
--    NotifyService.writeInApp), and a bare title let the next reset overwrite
--    an unread one about a different month.
--    The reason on the timeline (sent_back_reason) now says what happened —
--    added, edited or deleted, and on which date — instead of one fixed line.
--
-- 2. A race. MarkStaffReviewed counted the month's open rows and then wrote
--    the sign-off; a TA's draft committed between the two left a month
--    "reviewed" with a draft inside, and this trigger could not catch it
--    because at insert time the month was still 'pending'. The trigger now
--    takes the same per-cell advisory lock the staff transitions take
--    (lockPeriodCell: hashtextextended('submission_period_status/<period>/
--    <ta>/<course>', 7)) for every month it touches, whatever the status. The
--    sign-off and the send-back take that lock and count inside their own
--    transaction, so either they wait for the TA's write and see it, or the
--    TA's write waits for them and then resets what they signed.
CREATE OR REPLACE FUNCTION reset_staff_review_on_worklog_change() RETURNS trigger AS $$
DECLARE
    r        RECORD;
    v_reason TEXT;
    v_title  TEXT;
    v_body   TEXT;
    v_link   TEXT;
BEGIN
    IF TG_OP = 'UPDATE' AND ROW(OLD.*) IS NOT DISTINCT FROM ROW(NEW.*) THEN
        RETURN NULL;
    END IF;
    FOR r IN
        SELECT DISTINCT ON (sp.id, a.ta_id, tc.id)
               a.ta_id, tc.id AS tc_id, tc.code, sp.id AS period_id, sp.label,
               w.work_date,
               COALESCE(NULLIF(tp.prefix,''), NULLIF(u.title,''), '') ||
               COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,'') AS ta_name
          FROM (SELECT NEW.assignment_id AS assignment_id, NEW.work_date AS work_date
                 WHERE TG_OP <> 'DELETE'
                UNION
                SELECT OLD.assignment_id, OLD.work_date
                 WHERE TG_OP <> 'INSERT') w
          JOIN ta_request_assignments a ON a.id = w.assignment_id
          JOIN users u                  ON u.id = a.ta_id
          LEFT JOIN ta_profiles tp      ON tp.user_id = u.id
          JOIN sections sec             ON sec.id = a.section_id
          JOIN teaching_courses tc      ON tc.id = sec.teaching_course_id
          JOIN academic_terms trm       ON trm.id = tc.term_id
          JOIN submission_periods sp    ON sp.term_id = tc.term_id
           AND sp.year_month = trm.academic_year::text || '-' || to_char(w.work_date, 'MM')
         ORDER BY sp.id, a.ta_id, tc.id, w.work_date
    LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended(
            'submission_period_status/' || r.period_id::text || '/' || r.ta_id::text || '/' || r.tc_id::text, 7));

        v_reason := CASE TG_OP
                        WHEN 'INSERT' THEN 'มีการเพิ่มรายการวันที่ '
                        WHEN 'DELETE' THEN 'มีการลบรายการวันที่ '
                        ELSE 'มีการแก้ไขรายการวันที่ '
                    END
                    || to_char(r.work_date, 'FMDD/MM/') || (EXTRACT(YEAR FROM r.work_date)::int + 543)::text
                    || ' หลังเจ้าหน้าที่ตรวจแล้ว กรุณาตรวจอีกครั้ง';

        UPDATE submission_period_status SET
            status              = 'pending',
            staff_reviewed_by   = NULL,
            staff_reviewed_name = NULL,
            sent_back_at        = now(),
            sent_back_by        = NULL,
            sent_back_name      = 'ระบบ',
            sent_back_reason    = v_reason
         WHERE submission_period_id = r.period_id
           AND ta_id = r.ta_id
           AND teaching_course_id = r.tc_id
           AND status = 'staff_reviewed';

        IF FOUND THEN
            v_title := 'ต้องตรวจเบิกจ่ายอีกครั้ง ' || r.code || ' ' || r.ta_name || ' ' || r.label;
            v_body  := 'บันทึกเวลาปฏิบัติงานของ ' || r.ta_name || ' รายวิชา ' || r.code ||
                       ' ประจำเดือน' || r.label || ' ซึ่งเจ้าหน้าที่ตรวจสอบเบิกจ่ายแล้ว ' || v_reason ||
                       ' เดือนนี้จึงกลับไปรอการตรวจสอบ';
            v_link  := '/staff/payouts/' || r.tc_id::text;
            -- Same fold as NotifyService.writeInApp: an unread notice with this
            -- title and link is refreshed rather than repeated.
            UPDATE notifications n
               SET body = v_body, created_at = now()
             WHERE n.channel = 'in_app' AND n.read_at IS NULL
               AND n.title = v_title AND n.link IS NOT DISTINCT FROM v_link
               AND n.user_id IN (SELECT ur.user_id FROM user_roles ur WHERE ur.role = 'staff');
            INSERT INTO notifications (id, user_id, channel, title, body, link)
            SELECT gen_random_uuid(), su.id, 'in_app', v_title, v_body, v_link
              FROM users su
             WHERE su.is_active AND su.deleted_at IS NULL
               AND EXISTS (SELECT 1 FROM user_roles ur WHERE ur.user_id = su.id AND ur.role = 'staff')
               AND NOT EXISTS (SELECT 1 FROM notifications n
                                WHERE n.user_id = su.id AND n.channel = 'in_app'
                                  AND n.read_at IS NULL AND n.title = v_title
                                  AND n.link IS NOT DISTINCT FROM v_link);
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
