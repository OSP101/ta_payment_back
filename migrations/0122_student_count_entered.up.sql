-- Tell "not entered yet" (shown as "-") apart from "entered as 0" (nobody
-- enrolled). Both used to be stored as 0, so staff got a "ยังไม่กรอก" reminder
-- for courses they had deliberately set to 0.
--
-- One flag per track, set when staff save that track's count by hand (even as
-- 0). A track counts as entered when its count is > 0 OR its flag is set, so
-- imports, section edits and existing rows need no backfill.
ALTER TABLE teaching_courses
    ADD COLUMN IF NOT EXISTS num_students_regular_entered BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS num_students_special_entered BOOLEAN NOT NULL DEFAULT FALSE;
