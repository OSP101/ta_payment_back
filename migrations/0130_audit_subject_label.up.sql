-- 01/10/2026 — give every audit row a readable "what was this about".
--
-- audit_logs.entity_id is an id, and the audit screen showed it as one: eight
-- hex characters where a finance officer needed "CP363205 กลุ่ม 2" or a
-- person's name. The query behind the screen resolved two entity types (users
-- and teaching courses) with LEFT JOINs; the other twenty-odd fell through.
--
-- This function is the one place that knows how to name the thing an audit row
-- points at. It is resolved at READ time, on purpose:
--
--   * audit_logs refuses UPDATE at any age (0107/0108), so a stored label could
--     never be backfilled onto the rows that already exist — a read-time
--     resolver names old and new rows alike.
--   * A stored snapshot of a person's name would sit in an append-only table
--     for five years, out of reach of a PDPA erasure. Resolved live, the name
--     disappears from the trail the moment it disappears from the account.
--
-- The price is that a row about something since deleted has no label; the
-- caller falls back to the row's own before-image for those.
--
-- entity_id is free text (it also holds storage keys and "a/b" pairs), so the
-- id is only cast once it has been checked to LOOK like a uuid — and anything
-- unexpected returns NULL rather than raising, because this is called from the
-- query that lists the trail and one odd row must not take the page with it.
CREATE OR REPLACE FUNCTION audit_subject_label(p_entity TEXT, p_id TEXT) RETURNS TEXT AS $$
DECLARE
    v_id UUID;
    v    TEXT;
BEGIN
    IF p_id IS NULL OR p_id = '' THEN
        RETURN NULL;
    END IF;

    IF p_entity = 'curriculum' THEN
        SELECT COALESCE(NULLIF(full_name_th, ''), code) INTO v FROM curricula WHERE code = p_id;
        RETURN v;
    END IF;

    IF p_id !~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$' THEN
        RETURN NULL;
    END IF;
    v_id := p_id::uuid;

    CASE p_entity
    WHEN 'user', 'ta_profile' THEN
        SELECT NULLIF(TRIM(COALESCE(first_name, '') || ' ' || COALESCE(last_name, '')), '')
          INTO v FROM users WHERE id = v_id;
    WHEN 'teaching_course' THEN
        SELECT TRIM(code || ' ' || COALESCE(name_th, '')) INTO v FROM teaching_courses WHERE id = v_id;
    WHEN 'section' THEN
        SELECT tc.code || ' กลุ่ม ' || s.sec_no INTO v
          FROM sections s JOIN teaching_courses tc ON tc.id = s.teaching_course_id
         WHERE s.id = v_id;
    WHEN 'academic_term', 'term' THEN
        SELECT 'ภาค ' || semester || '/' || academic_year INTO v FROM academic_terms WHERE id = v_id;
    WHEN 'ta_document' THEN
        SELECT CASE d.kind
                   WHEN 'bank_book'     THEN 'สมุดบัญชีธนาคาร'
                   WHEN 'national_id'   THEN 'บัตรประชาชน'
                   WHEN 'creditor_form' THEN 'แบบฟอร์มเจ้าหนี้'
                   ELSE d.kind
               END || ' ของ ' || TRIM(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, ''))
          INTO v FROM ta_documents d JOIN users u ON u.id = d.user_id WHERE d.id = v_id;
    WHEN 'assignment', 'ta_request_assignment' THEN
        SELECT TRIM(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, ''))
               || ' · ' || tc.code || ' กลุ่ม ' || s.sec_no
          INTO v
          FROM ta_request_assignments a
          JOIN users u ON u.id = a.ta_id
          JOIN sections s ON s.id = a.section_id
          JOIN teaching_courses tc ON tc.id = s.teaching_course_id
         WHERE a.id = v_id;
    WHEN 'work_log' THEN
        SELECT TRIM(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, ''))
               || ' · ' || tc.code || ' · ' || to_char(w.work_date, 'DD/MM/YYYY')
          INTO v
          FROM work_logs w
          JOIN ta_request_assignments a ON a.id = w.assignment_id
          JOIN users u ON u.id = a.ta_id
          JOIN sections s ON s.id = a.section_id
          JOIN teaching_courses tc ON tc.id = s.teaching_course_id
         WHERE w.id = v_id;
    WHEN 'ta_review_schedule' THEN
        SELECT TRIM(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, ''))
               || ' · ' || tc.code || ' กลุ่ม ' || s.sec_no
          INTO v
          FROM ta_review_schedules r
          JOIN ta_request_assignments a ON a.id = r.assignment_id
          JOIN users u ON u.id = a.ta_id
          JOIN sections s ON s.id = a.section_id
          JOIN teaching_courses tc ON tc.id = s.teaching_course_id
         WHERE r.id = v_id;
    WHEN 'submission_period' THEN
        SELECT 'งวด ' || COALESCE(NULLIF(label, ''), year_month) INTO v FROM submission_periods WHERE id = v_id;
    WHEN 'submission_period_status' THEN
        SELECT TRIM(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, ''))
               || ' · ' || tc.code || ' · งวด ' || COALESCE(NULLIF(p.label, ''), p.year_month)
          INTO v
          FROM submission_period_status st
          JOIN users u ON u.id = st.ta_id
          JOIN teaching_courses tc ON tc.id = st.teaching_course_id
          JOIN submission_periods p ON p.id = st.submission_period_id
         WHERE st.id = v_id;
    WHEN 'worklog_edit_batch' THEN
        SELECT TRIM(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, ''))
               || ' · ' || tc.code || ' · เดือน ' || b.year_month
          INTO v
          FROM worklog_edit_batches b
          JOIN users u ON u.id = b.ta_id
          JOIN teaching_courses tc ON tc.id = b.teaching_course_id
         WHERE b.id = v_id;
    WHEN 'holiday' THEN
        SELECT name_th || ' (' || to_char(holiday_date, 'DD/MM/YYYY') || ')' INTO v
          FROM public_holidays WHERE id = v_id;
    WHEN 'admin_officer' THEN
        SELECT TRIM(COALESCE(title, '') || ' ' || COALESCE(full_name, '')) INTO v
          FROM admin_officers WHERE id = v_id;
    WHEN 'ta_request' THEN
        SELECT 'คำขอผู้ช่วยสอน ' || tc.code INTO v
          FROM ta_requests r JOIN teaching_courses tc ON tc.id = r.teaching_course_id
         WHERE r.id = v_id;
    WHEN 'ta_window' THEN
        SELECT 'ช่วงเปิดรับคำขอ ภาค ' || t.semester || '/' || t.academic_year INTO v
          FROM ta_request_windows w JOIN academic_terms t ON t.id = w.term_id
         WHERE w.id = v_id;
    WHEN 'announcement' THEN
        SELECT title INTO v FROM announcements WHERE id = v_id;
    WHEN 'pay_rate' THEN
        SELECT 'อัตราที่มีผล ' || to_char(effective_from, 'DD/MM/YYYY') INTO v FROM pay_rates WHERE id = v_id;
    WHEN 'export_batch' THEN
        SELECT file_name INTO v FROM export_batches WHERE id = v_id;
    WHEN 'appointment_order' THEN
        SELECT 'คำสั่งแต่งตั้ง ' || COALESCE(NULLIF(order_no, ''), 'รอบ ' || round_no) INTO v
          FROM appointment_orders WHERE id = v_id;
    WHEN 'course_summary_export' THEN
        SELECT 'สรุปงบรายวิชา ภาค ' || t.semester || '/' || t.academic_year INTO v
          FROM course_summary_exports e JOIN academic_terms t ON t.id = e.term_id
         WHERE e.id = v_id;
    WHEN 'transfer_cover_export' THEN
        SELECT 'ใบปะหน้าโอนเงิน ภาค ' || t.semester || '/' || t.academic_year INTO v
          FROM transfer_cover_exports e JOIN academic_terms t ON t.id = e.term_id
         WHERE e.id = v_id;
    WHEN 'data_deletion_request' THEN
        SELECT NULLIF(TRIM(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, '')), '') INTO v
          FROM data_deletion_requests r JOIN users u ON u.id = r.user_id
         WHERE r.id = v_id;
    WHEN 'ta_enrollment' THEN
        SELECT TRIM(COALESCE(u.first_name, '') || ' ' || COALESCE(u.last_name, '')) INTO v
          FROM ta_enrollments e JOIN users u ON u.id = e.user_id
         WHERE e.id = v_id;
    ELSE
        v := NULL;
    END CASE;

    RETURN NULLIF(TRIM(v), '');
EXCEPTION WHEN OTHERS THEN
    RETURN NULL;
END;
$$ LANGUAGE plpgsql STABLE;

COMMENT ON FUNCTION audit_subject_label(TEXT, TEXT) IS
    'Readable name for the thing an audit_logs row points at (entity, entity_id). Resolved live at read time; NULL when the thing is gone or the id is not one this function knows how to name.';
