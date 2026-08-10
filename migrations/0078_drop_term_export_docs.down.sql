CREATE TABLE term_export_docs (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id               UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    doc_kind              TEXT NOT NULL CHECK (doc_kind IN ('transfer_cover')),
    curriculum_code       TEXT REFERENCES curricula(code),
    doc_number            TEXT,
    doc_date              DATE,
    transferer_officer_id UUID REFERENCES admin_officers(id),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by            UUID REFERENCES users(id)
);

CREATE UNIQUE INDEX term_export_docs_scope_uidx
    ON term_export_docs (term_id, doc_kind, COALESCE(curriculum_code, ''));
