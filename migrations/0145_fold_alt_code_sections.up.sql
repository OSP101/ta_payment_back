-- A merged course is ONE class: fold each alternate-code section into the
-- primary section it is taught with.
--
-- 0111 opened every section of an alternate code as its own row
-- ("342372-1" beside "1"). Staff confirmed 02/10/2026 that these are the same
-- room, the same time, the same students' class — they want no separate
-- section at all. As separate rows they forced a lecturer to tick two boxes on
-- a TA request, made the TA generate and submit two sets of hours, and asked
-- for every holiday makeup twice (and a makeup filed on one copy only left the
-- other copy's original-date hours in place, so both could be billed).
--
-- Partner: the primary section (course_code IS NULL) of the same course with
-- the same bare section number and the same track — whatever its timetable:
-- an alternate section that meets at another time is still the same group
-- (office, 02/10/2026) and from now on meets on the partner's timetable.
-- Failing that, the course's ONLY section of the same track (registrar
-- numbering differs between codes: 342233 sec 1 ภาคพิเศษ is SC362005 sec 3,
-- its one special group). An alternate section with neither is left as it is;
-- staff fold it by hand (TeachingService.FoldSection), which runs the same
-- fold_section_into().
--
-- For each fold:
--   * the partner gains the alternate section's students (course totals are
--     unchanged: both rows were already summed into them);
--   * TA requests, assignments, hours, grading slots, makeups, review dates,
--     exam dates and TDBM links move to the partner; where the partner already
--     holds the same thing (the same TA on the same request, the same sitting,
--     the same makeup), the copy is dropped instead;
--   * the alternate section and its timetable are deleted — the class meets on
--     the partner's timetable.
--
-- Re-parenting work_logs is not an edit of hours: the month-lock, the
-- staff-review reset and the cross-course double-pay triggers are switched
-- off inside fold_section_into() and back on before it returns (it always
-- runs inside one transaction).

CREATE OR REPLACE FUNCTION fold_section_into(alt UUID, partner UUID) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    s RECORD;
    p RECORD;
    asg RECORD;
    keep UUID;
BEGIN
    SELECT id, teaching_course_id, track, num_students INTO s FROM sections WHERE id = alt;
    SELECT id, teaching_course_id, track INTO p FROM sections WHERE id = partner;
    IF s.id IS NULL OR p.id IS NULL OR s.id = p.id THEN
        RAISE EXCEPTION 'ไม่พบ section ที่จะรวม';
    END IF;
    IF s.teaching_course_id <> p.teaching_course_id THEN
        RAISE EXCEPTION 'รวมได้เฉพาะ section ในวิชาเดียวกัน';
    END IF;
    IF s.track <> p.track THEN
        RAISE EXCEPTION 'รวมได้เฉพาะ section ประเภทเดียวกัน (ภาคปกติกับภาคปกติ ภาคพิเศษกับภาคพิเศษ)';
    END IF;

    ALTER TABLE work_logs DISABLE TRIGGER work_logs_refuse_locked_month;
    ALTER TABLE work_logs DISABLE TRIGGER work_logs_reset_staff_review;
    ALTER TABLE work_logs DISABLE TRIGGER work_logs_refuse_cross_course_double_pay;

    UPDATE sections SET num_students = num_students + s.num_students WHERE id = partner;

    -- Requested TA counts: the partner's row for the same request keeps the larger.
    INSERT INTO ta_request_counts (request_id, section_id, undergrad_count, graduate_count)
    SELECT request_id, partner, undergrad_count, graduate_count
      FROM ta_request_counts WHERE section_id = alt
    ON CONFLICT (request_id, section_id) DO UPDATE
       SET undergrad_count = GREATEST(ta_request_counts.undergrad_count, EXCLUDED.undergrad_count),
           graduate_count  = GREATEST(ta_request_counts.graduate_count,  EXCLUDED.graduate_count);
    DELETE FROM ta_request_counts WHERE section_id = alt;

    -- Assignments: re-point, or merge into the partner's assignment of the
    -- same TA on the same request.
    FOR asg IN SELECT id, request_id, ta_id FROM ta_request_assignments WHERE section_id = alt LOOP
        keep := NULL;
        SELECT id INTO keep FROM ta_request_assignments
         WHERE request_id = asg.request_id AND section_id = partner AND ta_id = asg.ta_id;
        IF keep IS NULL THEN
            UPDATE ta_request_assignments SET section_id = partner WHERE id = asg.id;
        ELSE
            -- The same sitting logged on both copies is one sitting.
            DELETE FROM work_logs w
             WHERE w.assignment_id = asg.id
               AND EXISTS (SELECT 1 FROM work_logs k
                            WHERE k.assignment_id = keep AND k.work_date = w.work_date
                              AND k.start_time = w.start_time AND k.end_time = w.end_time
                              AND k.activity = w.activity);
            UPDATE work_logs SET assignment_id = keep WHERE assignment_id = asg.id;
            UPDATE work_log_lecturer_changes SET assignment_id = keep WHERE assignment_id = asg.id;
            INSERT INTO ta_review_schedules (assignment_id, kind, day_of_week, start_time, end_time, room, note)
            SELECT keep, kind, day_of_week, start_time, end_time, room, note
              FROM ta_review_schedules WHERE assignment_id = asg.id
            ON CONFLICT DO NOTHING;
            DELETE FROM ta_request_assignments WHERE id = asg.id; -- cascades its forms and slots
        END IF;
    END LOOP;

    -- Makeups move only for a class the partner actually holds on that
    -- weekday — an alternate section that met on another day has no class of
    -- the partner's to make up — and only where the partner has none yet.
    UPDATE makeup_schedules m SET section_id = partner
     WHERE m.section_id = alt
       AND EXISTS (SELECT 1 FROM section_schedules ss
                    WHERE ss.section_id = partner AND ss.kind = m.kind
                      AND ss.day_of_week = EXTRACT(DOW FROM m.original_date)::int)
       AND NOT EXISTS (SELECT 1 FROM makeup_schedules k
                        WHERE k.section_id = partner AND k.original_date = m.original_date AND k.kind = m.kind);
    DELETE FROM makeup_schedules WHERE section_id = alt;

    UPDATE lecture_review_dates r SET section_id = partner
     WHERE r.section_id = alt
       AND NOT EXISTS (SELECT 1 FROM lecture_review_dates k
                        WHERE k.section_id = partner AND k.review_date = r.review_date);
    DELETE FROM lecture_review_dates WHERE section_id = alt;

    UPDATE exam_schedules e SET section_id = partner
     WHERE e.section_id = alt
       AND NOT EXISTS (SELECT 1 FROM exam_schedules k WHERE k.section_id = partner AND k.kind = e.kind);
    DELETE FROM exam_schedules WHERE section_id = alt;

    UPDATE tdbm_extra_teachings SET section_id = partner WHERE section_id = alt;

    DELETE FROM sections WHERE id = alt; -- cascades its timetable

    -- A co-taught group that lost its copy may now hold one assignment; a
    -- group of one is no group.
    UPDATE ta_request_assignments a SET cotaught_group = NULL
     WHERE a.cotaught_group IS NOT NULL
       AND a.section_id = partner
       AND NOT EXISTS (SELECT 1 FROM ta_request_assignments b
                        WHERE b.request_id = a.request_id AND b.cotaught_group = a.cotaught_group
                          AND b.id <> a.id);

    ALTER TABLE work_logs ENABLE TRIGGER work_logs_refuse_locked_month;
    ALTER TABLE work_logs ENABLE TRIGGER work_logs_reset_staff_review;
    ALTER TABLE work_logs ENABLE TRIGGER work_logs_refuse_cross_course_double_pay;
END $$;

DO $$
DECLARE
    s RECORD;
    p_id UUID;
BEGIN
    FOR s IN
        SELECT id, teaching_course_id, track, substr(sec_no, length(course_code) + 2) AS bare
          FROM sections
         WHERE course_code IS NOT NULL
    LOOP
        p_id := NULL;
        SELECT p.id INTO p_id
          FROM sections p
         WHERE p.teaching_course_id = s.teaching_course_id
           AND p.course_code IS NULL
           AND ltrim(p.sec_no, '0') = ltrim(s.bare, '0') -- "01" and "1" are one section
           AND p.track = s.track;
        IF p_id IS NULL THEN
            SELECT min(p.id::text)::uuid INTO p_id
              FROM sections p
             WHERE p.teaching_course_id = s.teaching_course_id
               AND p.course_code IS NULL AND p.track = s.track
            HAVING COUNT(*) = 1;
        END IF;
        IF p_id IS NOT NULL THEN
            PERFORM fold_section_into(s.id, p_id);
        END IF;
    END LOOP;
END $$;
