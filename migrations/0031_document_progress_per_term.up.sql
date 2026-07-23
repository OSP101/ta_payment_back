-- 0031_document_progress_per_term.up.sql
-- Re-scope the physical document-progress tracker from per-course to
-- per-TERM: the whole term's paperwork travels as ONE batch through the
-- signature/routing journey, and the board only becomes actionable once EVERY
-- course in the term has been exported. 0030's per-course table is dropped
-- (the feature was new; no real data yet).

DROP TABLE IF EXISTS document_progress;

CREATE TABLE document_progress (
    term_id             UUID PRIMARY KEY REFERENCES academic_terms(id) ON DELETE CASCADE,
    stage               INT  NOT NULL DEFAULT 0 CHECK (stage BETWEEN 0 AND 5),
    ta_signed_at        TIMESTAMPTZ,
    lecturer_signed_at  TIMESTAMPTZ,
    certifier_signed_at TIMESTAMPTZ,
    sent_finance_at     TIMESTAMPTZ,
    sent_treasury_at    TIMESTAMPTZ,
    note                TEXT,
    updated_by          UUID REFERENCES users(id),
    updated_by_name     TEXT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
