-- 27/08/2026 — staff asked (relaying advice from a lecturer) that
-- "สรุปรายวิชาที่ขอใช้ TA" get the same generation ledger ปะหน้าจ่ายตรง already
-- has (transfer_cover_exports, 0077): every download currently re-renders from
-- whatever the tables say RIGHT NOW, so if a student count or TA approval
-- changes after a file has already gone out, there is no way to get back the
-- exact numbers that were actually printed and handed to whoever received it.
--
-- Same shape and same reasoning as transfer_cover_exports: freeze the render
-- input as a JSONB snapshot at generation time, re-render deterministically on
-- reprint — never re-query the (by then moved-on) tables. This document has no
-- PII to exclude (unlike ปะหน้าจ่ายตรง's PromptPay column), so the whole thing
-- can be frozen, not just part of it.
CREATE TABLE course_summary_exports (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id      UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    generated_by UUID REFERENCES users(id),
    generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    course_count INT NOT NULL,
    document     JSONB NOT NULL
);

CREATE INDEX course_summary_exports_term_idx
    ON course_summary_exports (term_id, generated_at DESC);

COMMENT ON COLUMN course_summary_exports.document IS
    'Frozen snapshot of every sheet''s course blocks (code, name, credits, lecturer, student counts, budget figures, TAs) — enough to re-render the exact xlsx bytes without touching the database again.';
