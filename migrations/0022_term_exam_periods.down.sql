ALTER TABLE academic_terms
    DROP COLUMN IF EXISTS midterm_starts_on,
    DROP COLUMN IF EXISTS midterm_ends_on,
    DROP COLUMN IF EXISTS final_starts_on,
    DROP COLUMN IF EXISTS final_ends_on;

ALTER TABLE teaching_courses
    ADD COLUMN IF NOT EXISTS midterm_lecture_date DATE,
    ADD COLUMN IF NOT EXISTS midterm_lab_date     DATE,
    ADD COLUMN IF NOT EXISTS final_lecture_date   DATE,
    ADD COLUMN IF NOT EXISTS final_lab_date       DATE;
