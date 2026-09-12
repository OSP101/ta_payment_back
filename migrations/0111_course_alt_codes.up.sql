-- 12/09/2026 — one course, several registrar codes.
--
-- The registrar file for 1/2569 lists the same class under more than one code
-- ~25 times (INTERNETWORKING is CP353301 + SC313302, INTRODUCTION TO LAND USE
-- PLANNING is CP373042 + SC333034, …): a curriculum reorganisation leaves the
-- old and new code both open against the same lecturer, room and slot. Staff
-- have always treated such a pair as ONE course — student counts added
-- together, one budget — and the import now asks them, per same-name group,
-- whether to do the same. course_groups (0073) stays for the two export
-- documents and for merges decided after the fact; this is the import-time
-- answer that makes the budget itself come out right.
--
-- alt_codes: the other registrar codes the course is also open under. `code`
-- stays the primary (printed first). Uniqueness of a code within a term is
-- enforced in the service layer across BOTH columns — a plain UNIQUE cannot
-- reach into an array.
ALTER TABLE teaching_courses
    ADD COLUMN IF NOT EXISTS alt_codes TEXT[] NOT NULL DEFAULT '{}';

-- course_code: which registrar code a section was opened under. NULL = the
-- course's own primary code (every pre-existing row). Sections brought in
-- under an alternate code keep the code in their sec_no too ("SC313302-01"),
-- so every screen and printed document that shows sec_no already tells the
-- two "sec 01"s apart without a schema-wide label change.
ALTER TABLE sections
    ADD COLUMN IF NOT EXISTS course_code TEXT;
