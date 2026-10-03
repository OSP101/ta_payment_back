-- tdbm_extra_teachings points at the course, section and auto-filled makeup it
-- was matched to (0116-0118) with no ON DELETE rule, so deleting a course that
-- TDBM had touched failed on the makeup the delete cascades to (03/10/2026:
-- "ลบไม่ได้ เพราะยังมีข้อมูลภาระงานสอนที่นำเข้าจาก TDBM อ้างอิงอยู่").
-- These are matches, not facts: the TDBM row itself is the record, and the
-- sync re-matches it. Losing the course or makeup only unmatches the row.
DO $$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT con.conname, att.attname
          FROM pg_constraint con
          JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = ANY (con.conkey)
         WHERE con.conrelid = 'tdbm_extra_teachings'::regclass
           AND con.contype = 'f'
           AND att.attname IN ('teaching_course_id', 'section_id', 'applied_makeup_id')
    LOOP
        EXECUTE format('ALTER TABLE tdbm_extra_teachings DROP CONSTRAINT %I', r.conname);
    END LOOP;
END $$;

ALTER TABLE tdbm_extra_teachings
    ADD CONSTRAINT tdbm_extra_teachings_teaching_course_id_fkey
        FOREIGN KEY (teaching_course_id) REFERENCES teaching_courses(id) ON DELETE SET NULL,
    ADD CONSTRAINT tdbm_extra_teachings_section_id_fkey
        FOREIGN KEY (section_id) REFERENCES sections(id) ON DELETE SET NULL,
    ADD CONSTRAINT tdbm_extra_teachings_applied_makeup_id_fkey
        FOREIGN KEY (applied_makeup_id) REFERENCES makeup_schedules(id) ON DELETE SET NULL;
