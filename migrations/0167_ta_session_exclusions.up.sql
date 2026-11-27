-- A cross-course clash removes only the conflicting meeting from the NEW
-- assignment. Keep this decision so it cannot reserve time or generate pay.
CREATE TABLE ta_assignment_session_exclusions (
    assignment_id UUID NOT NULL REFERENCES ta_request_assignments(id) ON DELETE CASCADE,
    schedule_id UUID NOT NULL REFERENCES section_schedules(id) ON DELETE CASCADE,
    reason TEXT NOT NULL,
    PRIMARY KEY (assignment_id, schedule_id)
);
CREATE VIEW ta_assignment_schedules AS
SELECT a.id AS assignment_id, ss.*
FROM ta_request_assignments a
JOIN section_schedules ss ON ss.section_id = a.section_id
LEFT JOIN ta_workload_forms w ON w.assignment_id = a.id
WHERE a.state <> 'dropped'
  -- Off-slot grading/preparation does not reserve a classroom meeting.
  -- Missing workload forms retain the conservative legacy behavior.
  AND (w.id IS NULL
       OR (a.level = 'undergrad' AND
           ((ss.kind = 'lecture' AND w.attendance_hrs > 0)
            OR (ss.kind = 'lab' AND w.lab_hrs > 0)))
       OR (a.level IN ('master', 'phd') AND w.help_teach_hrs > 0))
  AND NOT EXISTS (SELECT 1 FROM ta_assignment_session_exclusions e
                  WHERE e.assignment_id = a.id AND e.schedule_id = ss.id);
