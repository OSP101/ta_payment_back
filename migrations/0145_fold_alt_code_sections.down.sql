-- The folds themselves are not reversible: the folded sections, their
-- timetables and the per-code student split no longer exist to restore.
DROP FUNCTION IF EXISTS fold_section_into(UUID, UUID);
