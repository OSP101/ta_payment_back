-- Merge the central course catalog into the per-term offering.
--
-- Motivation:
--   The faculty no longer maintains a separate "รายวิชาของคณะ" catalog. Each term
--   the registrar Excel ("รายวิชาที่เปิดสอน") is imported as the single source of
--   truth, so a teaching_course must stand on its own — carrying the course
--   identity (code, names, credit hours, level) that used to live in
--   faculty_courses. This denormalizes those columns onto teaching_courses,
--   re-keys uniqueness as (term_id, code), and retires the catalog table.
--
-- Order matters: backfill runs while the FK still exists, before NOT NULL.

-- 1. Identity columns, nullable so the backfill can populate existing rows.
ALTER TABLE teaching_courses
    ADD COLUMN IF NOT EXISTS code        TEXT,
    ADD COLUMN IF NOT EXISTS name_th     TEXT,
    ADD COLUMN IF NOT EXISTS name_en     TEXT,
    ADD COLUMN IF NOT EXISTS level       TEXT,
    ADD COLUMN IF NOT EXISTS credits     INT,
    ADD COLUMN IF NOT EXISTS lecture_hrs INT,
    ADD COLUMN IF NOT EXISTS lab_hrs     INT,
    ADD COLUMN IF NOT EXISTS self_hrs    INT,
    ADD COLUMN IF NOT EXISTS department  TEXT;

-- 2. Backfill identity from the catalog via the soon-to-be-dropped FK.
UPDATE teaching_courses tc
SET code        = fc.code,
    name_th     = fc.name_th,
    name_en     = fc.name_en,
    level       = fc.level,
    credits     = fc.credits,
    lecture_hrs = fc.lecture_hrs,
    lab_hrs     = fc.lab_hrs,
    self_hrs    = fc.self_hrs,
    department  = fc.department
FROM faculty_courses fc
WHERE fc.id = tc.faculty_course_id;

-- 3. Mirror the old catalog constraints. credits/hrs default 0 so the importer
--    can insert a bare course; name_th/code stay required.
ALTER TABLE teaching_courses
    ALTER COLUMN code        SET NOT NULL,
    ALTER COLUMN name_th     SET NOT NULL,
    ALTER COLUMN level       SET DEFAULT 'undergrad',
    ALTER COLUMN level       SET NOT NULL,
    ALTER COLUMN credits     SET DEFAULT 0,
    ALTER COLUMN credits     SET NOT NULL,
    ALTER COLUMN lecture_hrs SET DEFAULT 0,
    ALTER COLUMN lecture_hrs SET NOT NULL,
    ALTER COLUMN lab_hrs     SET DEFAULT 0,
    ALTER COLUMN lab_hrs     SET NOT NULL,
    ALTER COLUMN self_hrs    SET DEFAULT 0,
    ALTER COLUMN self_hrs    SET NOT NULL;

ALTER TABLE teaching_courses
    ADD CONSTRAINT teaching_courses_level_check
        CHECK (level IN ('undergrad', 'graduate'));

-- 4. Drop the old catalog FK + composite UNIQUE + the FK column.
ALTER TABLE teaching_courses
    DROP CONSTRAINT IF EXISTS teaching_courses_faculty_course_id_term_id_key;
ALTER TABLE teaching_courses
    DROP CONSTRAINT IF EXISTS teaching_courses_faculty_course_id_fkey;
ALTER TABLE teaching_courses
    DROP COLUMN IF EXISTS faculty_course_id;

-- 5. A course code is unique within a term (was implied by the old global
--    UNIQUE(faculty_courses.code) + UNIQUE(faculty_course_id, term_id)).
ALTER TABLE teaching_courses
    ADD CONSTRAINT teaching_courses_term_id_code_key UNIQUE (term_id, code);

-- 6. Retire the catalog table (nothing else references it).
DROP TABLE faculty_courses;
