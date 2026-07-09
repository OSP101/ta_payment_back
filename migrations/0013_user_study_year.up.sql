-- Adds an explicit undergraduate year-of-study so the WBA (work-based) rule can
-- be enforced precisely: WBA class-schedule rows are only valid for year-4
-- undergraduate TAs. Before this, the backend could only check study_level, so
-- any undergrad could mark rows WBA to bypass the schedule-conflict gate.
--
-- NULL = unknown / not applicable (graduate, staff, lecturer). For undergrad
-- TAs, staff set this to the student's current year (typically 1..4).
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS study_year SMALLINT
    CHECK (study_year IS NULL OR (study_year >= 1 AND study_year <= 8));
