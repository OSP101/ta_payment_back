-- Course-level exam dates. Frontend "open course" flow captures these when
-- opening a course so that all sections inherit the same midterm/final date
-- per track (lecture / lab). TA worklog entries are gated on these being set.
ALTER TABLE teaching_courses
    ADD COLUMN IF NOT EXISTS midterm_lecture_date DATE,
    ADD COLUMN IF NOT EXISTS midterm_lab_date     DATE,
    ADD COLUMN IF NOT EXISTS final_lecture_date   DATE,
    ADD COLUMN IF NOT EXISTS final_lab_date       DATE;
