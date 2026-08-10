-- 09/08/2026 follow-up from the staff interview on the two hand-built payout
-- documents ("สรุปรายวิชาที่ขอใช้ TA" and "ปะหน้าจ่ายตรง"). Both are workbooks
-- with one sheet per curriculum, and both need a course that legitimately has
-- more than one registrar code to appear as a SINGLE row with its money added
-- together, not counted once per code. Two pieces of schema, used together.

-- ---------------------------------------------------------------------------
-- 1. curricula — the sheet identity a curriculum prints under.
--
-- sections.curriculum (added in 0068) is a derived programme CODE
-- (CS/IT/GIS/AI/CY/OTHER) used for dashboard grouping. It is not what goes on
-- a printed sheet tab: the college renamed the IT programme to "ITII" without
-- touching the derivation logic or the 67 existing rows tagged 'IT', so the
-- CODE and the SHEET NAME have to be allowed to disagree. This table is that
-- mapping, editable from the settings page instead of hardcoded, because a
-- programme rename is exactly the kind of thing that will happen again.
--
-- Two rows (CS_GRAD, DSAI_GRAD) exist for graduate curricula that have no
-- matching value in sections.curriculum at all — "สาขาวิชาวิทยาการคอมพิวเตอร์และ
-- เทคโนโลยีสารสนเทศ" (CS&IT) and "สาขาวิชาวิทยาการข้อมูลและปัญญาประดิษฐ์" (DS&AI)
-- serve CP388xxx/CP398xxx courses that are not imported yet (see the plan doc's
-- Phase 4). They are seeded now so the sheet identity exists ahead of the
-- courses; how a graduate-level course picks CS_GRAD over the undergrad CS row
-- is application logic in the export builder (BOTH sections.curriculum AND
-- teaching_courses.level matter there), not something a foreign key can express,
-- since one sections.curriculum value can route to two different curricula rows
-- depending on level.
CREATE TABLE curricula (
    code         TEXT PRIMARY KEY,
    sheet_name   TEXT NOT NULL,
    full_name_th TEXT NOT NULL,
    level        TEXT NOT NULL DEFAULT 'undergrad'
                     CHECK (level IN ('undergrad','graduate')),
    sort_order   INT NOT NULL DEFAULT 0
);

INSERT INTO curricula (code, sheet_name, full_name_th, level, sort_order) VALUES
    ('CS',       'CS',    'สาขาวิชาวิทยาการคอมพิวเตอร์',                       'undergrad', 1),
    ('IT',       'ITII',  'สาขาวิชาเทคโนโลยีสารสนเทศและนวัตกรรมอัจฉริยะ',        'undergrad', 2),
    ('GIS',      'GIS',   'สาขาวิชาภูมิสารสนเทศศาสตร์',                        'undergrad', 3),
    ('AI',       'AI',    'สาขาวิชาปัญญาประดิษฐ์',                            'undergrad', 4),
    ('CY',       'CY',    'สาขาวิชาความมั่นคงปลอดภัยไซเบอร์',                   'undergrad', 5),
    ('KKBS',     'KKBS',  'วิชาบริการคณะบริหารธุรกิจ',                         'undergrad', 6),
    ('OTHER',    'อื่นๆ', 'คณะอื่น ๆ',                                        'undergrad', 7),
    ('CS_GRAD',  'CS&IT', 'สาขาวิชาวิทยาการคอมพิวเตอร์และเทคโนโลยีสารสนเทศ',     'graduate',  8),
    ('DSAI_GRAD','DS&AI', 'สาขาวิชาวิทยาการข้อมูลและปัญญาประดิษฐ์',              'graduate',  9);

-- KKBS (CP020002/CP020003 in 1/2569) is a service course reserved for another
-- faculty's students, so curriculumFromReserved() already tags it OTHER
-- (correctly — the registrar's ReservedFor token is not SC-/CP-). Printing it
-- on its own sheet instead of the generic OTHER bucket is a staff decision, not
-- something the import can infer (OTHER covers every non-SC/CP faculty, KKBS is
-- one specific one) — so 'KKBS' has to be a value staff can hand-set on the
-- section, same as any other override the 0068 comment already promised.
ALTER TABLE sections DROP CONSTRAINT sections_curriculum_check;
ALTER TABLE sections ADD CONSTRAINT sections_curriculum_check
    CHECK (curriculum IN ('CS','IT','GIS','AI','CY','OTHER','KKBS'));

-- ---------------------------------------------------------------------------
-- 2. course_groups — courses that are the same class under more than one
--    registrar code (curriculum reorganisations leave old and new codes
--    running side by side against the same lecturer, room and time slot).
--
-- Detection (see detectCourseGroups in the service layer) proposes groups from
-- matching name + coinciding schedule; nothing here is written automatically.
-- A group only affects money once confirmed_by is set — the two export
-- documents merge a group's teaching_courses into one printed row with their
-- money ADDED (not recomputed from combined student counts, which the staff
-- interview confirmed), and skip printing its members individually.
CREATE TABLE course_groups (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id           UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    -- Decides which curriculum's sheet the merged row prints under, and whose
    -- code/name/lecturer are shown first — a group can legitimately span two
    -- curricula (CP353301=CS merged with SC313302=IT prints under CS in the
    -- college's own file).
    primary_course_id UUID NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    curriculum_code   TEXT NOT NULL REFERENCES curricula(code),
    confirmed_by      UUID REFERENCES users(id),
    confirmed_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The primary course is always also a member (enforced in the service layer,
-- not the schema — a CHECK can't reach into a sibling table). Membership is
-- exclusive: a course cannot be merged into two groups at once, or its money
-- would print twice.
CREATE TABLE course_group_members (
    course_group_id    UUID NOT NULL REFERENCES course_groups(id) ON DELETE CASCADE,
    teaching_course_id UUID NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    PRIMARY KEY (course_group_id, teaching_course_id)
);
CREATE UNIQUE INDEX course_group_members_course_uidx ON course_group_members (teaching_course_id);

CREATE INDEX course_groups_term_idx ON course_groups (term_id);
