-- TDBM added `section` ("กลุ่มที่ N") and `semester_type` ("ภาคปกติ"/"ภาคพิเศษ")
-- to /extra-teachings on top of the course_code fix from 0116 (2026-09-15).
-- These map onto our own schema exactly:
--   section       -> sections.sec_no    ("กลุ่มที่ 2" carries the number "2")
--   semester_type -> sections.track     ('ภาคปกติ'=regular, 'ภาคพิเศษ'=special)
-- so a submission can now be resolved down to one specific `sections` row
-- instead of just the course — see TDBMService.resolveSectionMatches.
ALTER TABLE tdbm_extra_teachings
    ADD COLUMN section_label TEXT,
    ADD COLUMN semester_type TEXT,
    -- Nullable, separate from teaching_course_id: a row can match the course
    -- but not (yet, or ever) a specific section — e.g. section_label doesn't
    -- parse, or names a group our sections table doesn't have. Keeping both
    -- columns lets staff tell "matched course, unmatched section" apart from
    -- "matched nothing at all" instead of collapsing both to one NULL.
    ADD COLUMN section_id UUID REFERENCES sections(id);

CREATE INDEX idx_tdbm_extra_teachings_section ON tdbm_extra_teachings (section_id);
