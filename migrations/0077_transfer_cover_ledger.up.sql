-- 09/08/2026 — ปะหน้าจ่ายตรง is a finance document: staff need to be able to
-- answer "who generated it, when, for how much" after the fact, and hand back
-- an exact reprint rather than a recomputed one that may have quietly moved if
-- a work log changed since. Same shape as appointment_orders (0043/0060): a
-- JSONB snapshot of what was actually printed, frozen at generation time, so
-- Reprint renders the snapshot instead of re-querying tables that keep moving.
--
-- One row per generation covering every curriculum/track sheet the term had
-- data for — not one row per sheet — because staff think of "the transfer
-- cover for this term" as a single act, and the total_baht column is the
-- one grand total across every sheet in that file.
CREATE TABLE transfer_cover_exports (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id      UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    generated_by UUID REFERENCES users(id),
    generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    total_baht   NUMERIC(12,2) NOT NULL,
    sheet_count  INT NOT NULL,
    document     JSONB NOT NULL
);

CREATE INDEX transfer_cover_exports_term_idx
    ON transfer_cover_exports (term_id, generated_at DESC);

COMMENT ON COLUMN transfer_cover_exports.document IS
    'Frozen snapshot of every sheet''s rows (name, courses, baht, promptpay, seniority note) plus the header labels used — enough to re-render the exact xlsx bytes without touching the database again.';
