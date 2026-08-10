DROP TABLE IF EXISTS course_group_members;
DROP TABLE IF EXISTS course_groups;

ALTER TABLE sections DROP CONSTRAINT sections_curriculum_check;
ALTER TABLE sections ADD CONSTRAINT sections_curriculum_check
    CHECK (curriculum IN ('CS','IT','GIS','AI','CY','OTHER'));

DROP TABLE IF EXISTS curricula;
