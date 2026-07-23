-- Per-TA weekly review pattern (วันตรวจการบ้าน). One row per recurring slot
-- the TA plans to spend grading. Auto-generate expands these across the term
-- exactly like section_schedules does for lecture/lab, but scoped to the TA's
-- assignment so different TAs on the same section can pick different days.
--
-- Review activities are NOT shifted by makeup (Q&A rule 7) — the pattern
-- lives on the ORIGINAL weekday.
CREATE TABLE ta_review_schedules (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  assignment_id   uuid NOT NULL REFERENCES ta_request_assignments(id) ON DELETE CASCADE,
  day_of_week     int  NOT NULL CHECK (day_of_week BETWEEN 0 AND 6),
  start_time      time NOT NULL,
  end_time        time NOT NULL,
  room            text,
  note            text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  CHECK (end_time > start_time),
  UNIQUE (assignment_id, day_of_week, start_time, end_time)
);

CREATE INDEX ix_trs_assignment ON ta_review_schedules(assignment_id);
