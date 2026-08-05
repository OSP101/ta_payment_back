-- Collapse the per-person rows back into one row per course-role.
--
-- A course-role counts as signed only when EVERY person on it had signed, which
-- is what the lumped row meant. Anything partial reverts to unsigned — the old
-- shape has no way to say "two of four".
INSERT INTO signature_checklist
    (term_id, teaching_course_id, role, signed_at, updated_by, updated_by_name, updated_at)
SELECT term_id, teaching_course_id, role,
       CASE WHEN COUNT(*) FILTER (WHERE signed_at IS NULL) = 0
            THEN MAX(signed_at) END,
       MAX(updated_by::text)::uuid, MAX(updated_by_name), MAX(updated_at)
FROM signature_checklist
WHERE signer_id IS NOT NULL
GROUP BY term_id, teaching_course_id, role;

DELETE FROM signature_checklist WHERE signer_id IS NOT NULL;

DROP INDEX IF EXISTS ux_signature_checklist_person;
DROP INDEX IF EXISTS ux_signature_checklist_course_role;

ALTER TABLE signature_checklist
    DROP COLUMN signer_id,
    DROP COLUMN signer_name;

ALTER TABLE signature_checklist
    ADD CONSTRAINT signature_checklist_teaching_course_id_role_key
    UNIQUE (teaching_course_id, role);
