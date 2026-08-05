-- One signature row per PERSON, not one per course-role.
--
-- The checklist used to hold three rows per course — ta / lecturer / certifier —
-- where the "ta" row stood for every TA on the course at once. Staff could only
-- record "all the TAs signed", never "two of four have". The physical document
-- is signed one hand at a time, so the board could not answer the only question
-- the officer actually has: who is still missing.
--
-- signer_id is the specific person for ta/lecturer. It stays NULL for certifier:
-- the certifier is an admin_officers row (see ResolveCertifier), not a user, and
-- there is exactly one per course, so the course-role pair still identifies it.
ALTER TABLE signature_checklist
    ADD COLUMN signer_id   UUID REFERENCES users(id) ON DELETE CASCADE,
    ADD COLUMN signer_name TEXT;

-- The old uniqueness has to go FIRST. It is UNIQUE (teaching_course_id, role),
-- so the second expanded row of any course would collide with the first — the
-- expansion below cannot even start while it stands.
ALTER TABLE signature_checklist DROP CONSTRAINT IF EXISTS signature_checklist_teaching_course_id_role_key;
DROP INDEX IF EXISTS signature_checklist_teaching_course_id_role_key;

-- Expand each existing lumped row into one row per person, carrying its state.
-- A signed lump meant "everybody signed", so every expanded row inherits that
-- timestamp; an unsigned lump expands to unsigned rows.
INSERT INTO signature_checklist
    (term_id, teaching_course_id, role, signer_id, signer_name,
     signed_at, updated_by, updated_by_name, updated_at)
SELECT sc.term_id, sc.teaching_course_id, 'ta', u.id,
       COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,''),
       sc.signed_at, sc.updated_by, sc.updated_by_name, sc.updated_at
FROM signature_checklist sc
JOIN sections s               ON s.teaching_course_id = sc.teaching_course_id
JOIN ta_request_assignments a ON a.section_id = s.id AND a.state <> 'dropped'
JOIN ta_requests r            ON r.id = a.request_id AND r.status = 'approved'
JOIN users u                  ON u.id = a.ta_id
WHERE sc.role = 'ta' AND sc.signer_id IS NULL
GROUP BY sc.term_id, sc.teaching_course_id, u.id, u.first_name, u.last_name,
         sc.signed_at, sc.updated_by, sc.updated_by_name, sc.updated_at;

-- The lecturer stage belongs to whoever SUBMITTED the request, not to everyone
-- listed as teaching the course — a co-teacher who filed nothing has nothing to
-- sign, and listing them left the officer chasing the wrong person.
INSERT INTO signature_checklist
    (term_id, teaching_course_id, role, signer_id, signer_name,
     signed_at, updated_by, updated_by_name, updated_at)
SELECT sc.term_id, sc.teaching_course_id, 'lecturer', u.id,
       COALESCE(u.first_name,'') || ' ' || COALESCE(u.last_name,''),
       sc.signed_at, sc.updated_by, sc.updated_by_name, sc.updated_at
FROM signature_checklist sc
JOIN ta_requests r ON r.teaching_course_id = sc.teaching_course_id AND r.status = 'approved'
JOIN users u       ON u.id = r.lecturer_id
WHERE sc.role = 'lecturer' AND sc.signer_id IS NULL
GROUP BY sc.term_id, sc.teaching_course_id, u.id, u.first_name, u.last_name,
         sc.signed_at, sc.updated_by, sc.updated_by_name, sc.updated_at;

-- Drop the now-expanded lumps. Certifier rows keep signer_id NULL and stay.
DELETE FROM signature_checklist
WHERE signer_id IS NULL AND role IN ('ta', 'lecturer');

-- Two partial uniques rather than one over (course, role, signer_id): NULLs do
-- not collide in a plain unique index, so the certifier row would be free to
-- duplicate itself on every toggle.
CREATE UNIQUE INDEX ux_signature_checklist_person
    ON signature_checklist (teaching_course_id, role, signer_id)
    WHERE signer_id IS NOT NULL;
CREATE UNIQUE INDEX ux_signature_checklist_course_role
    ON signature_checklist (teaching_course_id, role)
    WHERE signer_id IS NULL;
