-- Reverse of 0039: rebuild the faculty_courses catalog from the denormalized
-- teaching_courses rows and re-point the FK. Lossy — per-term drift in a course
-- code collapses to one catalog row (latest updated wins).

CREATE TABLE faculty_courses (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code         TEXT NOT NULL UNIQUE,
    name_th      TEXT NOT NULL,
    name_en      TEXT,
    credits      INT NOT NULL,
    lecture_hrs  INT NOT NULL DEFAULT 0,
    lab_hrs      INT NOT NULL DEFAULT 0,
    self_hrs     INT NOT NULL DEFAULT 0,
    department   TEXT,
    level        TEXT NOT NULL DEFAULT 'undergrad'
                 CHECK (level IN ('undergrad', 'graduate')),
    is_active    BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO faculty_courses (code, name_th, name_en, credits, lecture_hrs, lab_hrs, self_hrs, department, level)
SELECT DISTINCT ON (code)
       code, name_th, name_en, credits, lecture_hrs, lab_hrs, self_hrs, department, level
FROM teaching_courses
ORDER BY code, updated_at DESC;

ALTER TABLE teaching_courses ADD COLUMN faculty_course_id UUID;
UPDATE teaching_courses tc
SET faculty_course_id = fc.id
FROM faculty_courses fc
WHERE fc.code = tc.code;

ALTER TABLE teaching_courses
    ALTER COLUMN faculty_course_id SET NOT NULL,
    ADD CONSTRAINT teaching_courses_faculty_course_id_fkey
        FOREIGN KEY (faculty_course_id) REFERENCES faculty_courses(id),
    DROP CONSTRAINT teaching_courses_term_id_code_key,
    ADD CONSTRAINT teaching_courses_faculty_course_id_term_id_key
        UNIQUE (faculty_course_id, term_id),
    DROP CONSTRAINT teaching_courses_level_check;

ALTER TABLE teaching_courses
    DROP COLUMN code,
    DROP COLUMN name_th,
    DROP COLUMN name_en,
    DROP COLUMN level,
    DROP COLUMN credits,
    DROP COLUMN lecture_hrs,
    DROP COLUMN lab_hrs,
    DROP COLUMN self_hrs,
    DROP COLUMN department;
