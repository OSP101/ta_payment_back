DROP INDEX curricula_sheet_name_unique;
ALTER TABLE sections DROP CONSTRAINT sections_curriculum_fkey;
-- Refuse rollback until custom section codes are reassigned.
ALTER TABLE sections ADD CONSTRAINT sections_curriculum_check CHECK (curriculum IN ('CS','IT','GIS','AI','CY','OTHER','KKBS'));
