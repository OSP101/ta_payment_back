-- Per-course signature checklist so staff can see (and everyone can read) which
-- course + role has not signed yet, instead of only the term-wide stepper
-- (document_progress, migration 0031). Complements — does not replace — that
-- term stepper. Roles: ta / lecturer / certifier.
CREATE TABLE signature_checklist (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id            UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    teaching_course_id UUID NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    role               TEXT NOT NULL CHECK (role IN ('ta', 'lecturer', 'certifier')),
    signed_at          TIMESTAMPTZ,
    updated_by         UUID REFERENCES users(id),
    updated_by_name    TEXT,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (teaching_course_id, role)
);
CREATE INDEX ix_signature_checklist_term ON signature_checklist(term_id);
