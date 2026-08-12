DROP INDEX IF EXISTS ux_signature_checklist_person;
DROP INDEX IF EXISTS ux_signature_checklist_course_role;
CREATE UNIQUE INDEX ux_signature_checklist_person
    ON signature_checklist (teaching_course_id, role, signer_id)
    WHERE signer_id IS NOT NULL;
CREATE UNIQUE INDEX ux_signature_checklist_course_role
    ON signature_checklist (teaching_course_id, role)
    WHERE signer_id IS NULL;
ALTER TABLE signature_checklist DROP COLUMN IF EXISTS fiscal_round;

ALTER TABLE document_progress DROP CONSTRAINT document_progress_pkey;
ALTER TABLE document_progress DROP COLUMN IF EXISTS fiscal_round;
ALTER TABLE document_progress ADD PRIMARY KEY (term_id);
