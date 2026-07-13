-- Move exam windows from per-course to per-term. The faculty publishes exam
-- ranges once at the term level; TA worklog entries falling inside either
-- range are rejected so payment isn't accrued during the exam blackout.
--
-- Fields are DATE ranges (inclusive both ends). Nullable to make backfilling
-- existing terms possible; the API enforces NOT NULL on create/update going
-- forward.
ALTER TABLE academic_terms
    ADD COLUMN IF NOT EXISTS midterm_starts_on DATE,
    ADD COLUMN IF NOT EXISTS midterm_ends_on   DATE,
    ADD COLUMN IF NOT EXISTS final_starts_on   DATE,
    ADD COLUMN IF NOT EXISTS final_ends_on     DATE;

-- Retire the per-course exam-date fields — they were only ever set through the
-- lecturer's "open course" modal and no business logic consumed them (see the
-- comment on 0007). Term-level ranges supersede them entirely.
ALTER TABLE teaching_courses
    DROP COLUMN IF EXISTS midterm_lecture_date,
    DROP COLUMN IF EXISTS midterm_lab_date,
    DROP COLUMN IF EXISTS final_lecture_date,
    DROP COLUMN IF EXISTS final_lab_date;
