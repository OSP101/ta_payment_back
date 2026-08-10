-- 09/08/2026 — the ปะหน้าจ่ายตรง (transfer-cover) sheet's header carries a
-- memo number, its date, and who signs "ผู้แจ้งโอน" — none of which exist
-- anywhere in the schema. These follow the same pattern as
-- academic_terms.certifier_officer_id (0061): a college-level appointment,
-- filled in once by staff, reused every time the document is generated.
--
-- Scoped per (term, doc_kind, curriculum) rather than just per term: the
-- college's own process sends one memo number per curriculum today (see
-- ปะหน้าจ่ายตรง-CY.xls's "เลขที่ อว 660301.26.6.2/ ง"), and Phase 3 may need
-- that granularity even though the sheets are now bundled into one workbook.
-- curriculum_code NULL means "one number for every curriculum this term" —
-- staff can fill that single row instead of nine, if that turns out to be how
-- the office actually wants to run it.
CREATE TABLE term_export_docs (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id               UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    -- Room to grow: only the transfer-cover sheet has a memo header today.
    doc_kind              TEXT NOT NULL CHECK (doc_kind IN ('transfer_cover')),
    curriculum_code       TEXT REFERENCES curricula(code),
    doc_number            TEXT,
    doc_date              DATE,
    transferer_officer_id UUID REFERENCES admin_officers(id),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by            UUID REFERENCES users(id)
);

-- COALESCE folds every curriculum_code-NULL row for the same (term, doc_kind)
-- into one uniqueness bucket — plain NULL columns never collide under a
-- standard unique index, which would have let staff create duplicates.
CREATE UNIQUE INDEX term_export_docs_scope_uidx
    ON term_export_docs (term_id, doc_kind, COALESCE(curriculum_code, ''));

COMMENT ON COLUMN term_export_docs.doc_number IS
    'The memo reference (เลขที่ อว ...) printed in the document header. Free text — the numbering scheme is the registrar''s, not ours.';
COMMENT ON COLUMN term_export_docs.transferer_officer_id IS
    'Who signs the ผู้แจ้งโอน block. Same officer roster as academic_terms.certifier_officer_id.';
