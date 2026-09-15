DROP INDEX IF EXISTS idx_tdbm_extra_teachings_section;

ALTER TABLE tdbm_extra_teachings
    DROP COLUMN IF EXISTS section_id,
    DROP COLUMN IF EXISTS semester_type,
    DROP COLUMN IF EXISTS section_label;
