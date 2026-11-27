ALTER TABLE sections DROP CONSTRAINT sections_curriculum_check;
ALTER TABLE sections ADD CONSTRAINT sections_curriculum_fkey FOREIGN KEY (curriculum) REFERENCES curricula(code) ON DELETE RESTRICT;
CREATE UNIQUE INDEX curricula_sheet_name_unique ON curricula (lower(sheet_name));
