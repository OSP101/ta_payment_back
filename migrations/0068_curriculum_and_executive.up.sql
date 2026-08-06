-- 06/08/2026 management-meeting follow-up: the staff dashboard grows an
-- executive view (budget by month, by curriculum, history across terms).
-- Two schema pieces travel together because both exist only for that view.
--
-- 1. Which curriculum a section serves. The registrar's ReservedFor column
--    already says this ("ตรี : SC-IT ปี 3 ขึ้นไป") and the import file matches
--    every course in the system 127/127, so the value is DERIVED at import
--    time, never hand-entered course by course. Stored per SECTION because one
--    course legitimately serves several curricula (20 courses in 1/2569 do).
--
--    Values are the normalised programme groups the college runs:
--      CS  = วิทยาการคอมพิวเตอร์      (SC-CS)
--      IT  = เทคโนโลยีสารสนเทศ        (SC-IT)
--      GIS = ภูมิสารสนเทศศาสตร์       (SC-GIS and CP-GIS — same programme, two code eras)
--      AI  = ปัญญาประดิษฐ์            (CP-AI)
--      CY  = ความมั่นคงปลอดภัยไซเบอร์  (CP-Cy)
--      OTHER = คณะอื่น ๆ — any prefix that is not SC/CP (BS-*, ...) means
--              students from another faculty enrol here; management asked for
--              these to be grouped separately, not guessed into a programme.
--    NULL = imported before this column existed and not yet backfilled — the
--    dashboard shows these as "ยังไม่ระบุ" rather than lumping them into OTHER,
--    because "we don't know" and "another faculty" are different answers.
ALTER TABLE sections
    ADD COLUMN IF NOT EXISTS curriculum TEXT
        CHECK (curriculum IN ('CS','IT','GIS','AI','CY','OTHER'));

COMMENT ON COLUMN sections.curriculum IS
    'Programme group served, derived from the import file''s ReservedFor (CS/IT/GIS/AI/CY, OTHER = non-SC/CP faculty). NULL = not yet known. Staff may override; re-import only fills NULLs so an override survives.';

-- 2. Executive access. The management team are all lecturers with existing
--    accounts, so this is a per-user flag staff tick in the users page, not a
--    fifth value in role_code (ALTER TYPE ... ADD VALUE cannot be used inside
--    the migration transaction that adds it — see 0041). rbac.Roles() surfaces
--    the flag as a synthetic "executive" role, read-only analytics only.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS is_executive BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN users.is_executive IS
    'Grants the read-only executive dashboard (budget analytics). Ticked per user by staff; independent of user_roles.';
