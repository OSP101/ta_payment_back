ALTER TABLE tdbm_extra_teachings
    DROP CONSTRAINT IF EXISTS tdbm_extra_teachings_teaching_course_id_fkey,
    DROP CONSTRAINT IF EXISTS tdbm_extra_teachings_section_id_fkey,
    DROP CONSTRAINT IF EXISTS tdbm_extra_teachings_applied_makeup_id_fkey;
ALTER TABLE tdbm_extra_teachings
    ADD CONSTRAINT tdbm_extra_teachings_teaching_course_id_fkey
        FOREIGN KEY (teaching_course_id) REFERENCES teaching_courses(id),
    ADD CONSTRAINT tdbm_extra_teachings_section_id_fkey
        FOREIGN KEY (section_id) REFERENCES sections(id),
    ADD CONSTRAINT tdbm_extra_teachings_applied_makeup_id_fkey
        FOREIGN KEY (applied_makeup_id) REFERENCES makeup_schedules(id);
