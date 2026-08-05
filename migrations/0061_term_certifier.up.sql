-- The ผู้รับรอง block on the claim form was printing the wrong person.
--
-- assets/templates/ta_claim_form.xlsx carries three signature blocks:
-- ผู้ปฏิบัติงาน (the TA), อาจารย์ผู้สอน (the lecturer), and ผู้รับรอง — whose
-- position line the template hardcodes as "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์".
-- The exporter filled the certifier's NAME cell with the lecturer's name, so
-- every claim went out attesting that the course's own lecturer certified it in
-- the capacity of head of department. Two different people, one of them wrong.
--
-- The certifier is a college-level appointment, not a per-course one: the same
-- person certifies every claim for a term. So it is stored on the term, and
-- resolved from the officer roster when unset.
ALTER TABLE academic_terms
    ADD COLUMN certifier_officer_id UUID REFERENCES admin_officers(id);

COMMENT ON COLUMN academic_terms.certifier_officer_id IS
    'Who signs the ผู้รับรอง block on this term''s claim forms. NULL = fall back to the active head of department on the officer roster.';
