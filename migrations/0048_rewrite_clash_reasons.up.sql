-- Rewrite the stored clash explanations in the new, specific wording.
--
-- The old sentence was a bare count:
--     "Section 1: 1 จาก 2 คาบตรงกับตารางเรียนของคุณ — คาบเหล่านั้นลงบันทึกเวลาไม่ได้"
-- which reads identically whether the TA lost the lecture hour or the lab hour.
-- Those are different jobs: losing a บรรยาย slot costs them the attendance-
-- taking for that hour, while losing a ปฏิบัติการ slot means they cannot run
-- the lab at all and are left with marking and attendance only.
--
-- ta_request_decide.go now writes the detailed form, but only when a request is
-- decided. Requests already approved keep the old text forever, so the two
-- wordings would sit side by side in the same list. This backfills them.
--
-- The text below must stay in step with clashLines() / applyClashOutcome() in
-- internal/service/ta_request_decide.go. It is duplicated rather than shared
-- because a migration cannot call Go, and it runs exactly once.
--
-- Rows whose clash is no longer computable (the TA has since removed the
-- colliding class) are left untouched: state_reason records why the decision
-- was made at the time, and inventing a new explanation from today's timetable
-- would misreport history. Those rows keep their original sentence.

WITH sessions AS (
    -- One row per (assignment, teaching session) that collides. DISTINCT ON
    -- because a single session colliding with two of the TA's own classes is
    -- still one session lost.
    SELECT DISTINCT ON (a.id, ss.id)
           a.id                                            AS assignment_id,
           ss.id                                           AS session_id,
           ss.kind                                         AS session_kind,
           ss.day_of_week,
           ss.start_time,
           ss.end_time,
           COALESCE(NULLIF(cs.course_code, ''), '')        AS own_code,
           COALESCE(NULLIF(cs.course_name, ''),
                    NULLIF(cs.course_label, ''), '')       AS own_name,
           COALESCE(cs.kind, '')                           AS own_kind
      FROM ta_request_assignments a
      JOIN sections           sec ON sec.id = a.section_id
      JOIN teaching_courses   tc  ON tc.id  = sec.teaching_course_id
      JOIN section_schedules  ss  ON ss.section_id = sec.id
      JOIN ta_class_schedules cs
        ON cs.user_id = a.ta_id
       AND NOT cs.is_wba
       AND cs.term_id     = tc.term_id
       AND cs.day_of_week = ss.day_of_week
       AND cs.start_time  < ss.end_time
       AND ss.start_time  < cs.end_time
     WHERE a.state IN ('trimmed', 'dropped')
     ORDER BY a.id, ss.id
),
lines AS (
    SELECT assignment_id,
           string_agg(
               '• '
               || CASE session_kind
                    WHEN 'lecture' THEN 'คาบบรรยาย'
                    WHEN 'lab'     THEN 'คาบปฏิบัติการ'
                    ELSE 'คาบสอน'
                  END
               || ' '
               || (ARRAY['อาทิตย์','จันทร์','อังคาร','พุธ','พฤหัสบดี','ศุกร์','เสาร์'])[day_of_week + 1]
               || ' ' || TO_CHAR(start_time, 'HH24:MI')
               || '–' || TO_CHAR(end_time,   'HH24:MI')
               || ' '
               || CASE
                    WHEN BTRIM(own_code || ' ' || own_name) = ''
                      THEN 'ตรงกับคาบเรียนของคุณ'
                    ELSE 'ตรงกับ '
                         || BTRIM(own_code || ' ' || own_name)
                         || CASE own_kind
                              WHEN 'lecture' THEN ' (บรรยาย)'
                              WHEN 'lab'     THEN ' (ปฏิบัติการ)'
                              ELSE ''
                            END
                         || ' ที่คุณเรียน'
                  END,
               E'\n' ORDER BY day_of_week, start_time)                AS body,
           COUNT(*)                                                    AS clashing
      FROM sessions
     GROUP BY assignment_id
),
totals AS (
    SELECT a.id                AS assignment_id,
           a.state::text       AS state,
           sec.sec_no,
           (SELECT COUNT(*) FROM section_schedules ss
             WHERE ss.section_id = a.section_id)            AS total,
           -- Session kinds with no collision — what the TA can still do.
           (SELECT string_agg(DISTINCT CASE ss.kind
                                         WHEN 'lecture' THEN 'บรรยาย'
                                         WHEN 'lab'     THEN 'ปฏิบัติการ'
                                       END, ' และ ')
              FROM section_schedules ss
             WHERE ss.section_id = a.section_id
               AND ss.kind IN ('lecture', 'lab')
               AND NOT EXISTS (
                   SELECT 1 FROM ta_class_schedules cs
                    JOIN sections sx ON sx.id = a.section_id
                    JOIN teaching_courses tc ON tc.id = sx.teaching_course_id
                    WHERE cs.user_id = a.ta_id
                      AND NOT cs.is_wba
                      AND cs.term_id     = tc.term_id
                      AND cs.day_of_week = ss.day_of_week
                      AND cs.start_time  < ss.end_time
                      AND ss.start_time  < cs.end_time))    AS remaining
      FROM ta_request_assignments a
      JOIN sections sec ON sec.id = a.section_id
     WHERE a.state IN ('trimmed', 'dropped')
)
UPDATE ta_request_assignments a
   SET state_reason =
       CASE
         WHEN t.state = 'dropped' THEN
              'Section ' || t.sec_no
              || ': ทุกคาบตรงกับตารางเรียนของคุณ — คุณจึงไม่ได้เป็นผู้ช่วยสอนกลุ่มนี้'
              || E'\n' || l.body
         ELSE
              'Section ' || t.sec_no
              || ': ตรงกับตารางเรียนของคุณ ' || l.clashing || ' จาก ' || t.total || ' คาบ'
              || E'\n' || l.body
              || CASE WHEN COALESCE(t.remaining, '') <> ''
                      THEN E'\n' || 'ยังลงเวลาในคาบ' || t.remaining || 'ได้ตามปกติ'
                      ELSE '' END
       END
  FROM lines l
  JOIN totals t ON t.assignment_id = l.assignment_id
 WHERE a.id = l.assignment_id;
