-- 08/09/2026 — a five-year retention for audit_logs, decided by the office.
--
-- Migration 0107 made the table append-only, which was right for evidence and
-- created two problems it did not solve:
--
--   1. The table grows forever. Nothing could ever be removed from it.
--   2. PDPA erasure could not reach it. A person's right to be forgotten
--      stopped at a table the application was forbidden to delete from.
--
-- Both are answered by an AGE, not by an exception. Thai practice keeps
-- financial records five years, the claim documents this trail describes are
-- financial records, and personal data in the trail therefore has a defined
-- end — which is what makes "we cannot delete it on request" a retention
-- policy rather than a refusal.

-- ── The rule lives in the database ──────────────────────────────────────────
-- The obvious implementation is to drop the trigger, purge, and put it back.
-- That is rejected: it opens a window in which every row in the table is
-- deletable, and the purge is exactly the moment something could go wrong
-- unnoticed. Instead the trigger itself learns the policy — a DELETE is allowed
-- ONLY for a row that has already aged past retention, so even a purge job with
-- a mistaken WHERE clause cannot reach anything recent, and no window ever
-- exists.
--
-- UPDATE stays refused unconditionally. Nothing legitimate rewrites history.
CREATE OR REPLACE FUNCTION audit_logs_append_only() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' AND OLD.at < NOW() - audit_retention() THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'audit_logs is append-only: % is not permitted (retention %)',
        TG_OP, audit_retention()
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

-- The period is a function rather than a literal in the trigger body so that
-- changing it is one obvious statement in one obvious place, and so the
-- application can ask the database what the policy is instead of keeping its
-- own copy that could drift.
CREATE OR REPLACE FUNCTION audit_retention() RETURNS INTERVAL AS $$
    SELECT INTERVAL '5 years';
$$ LANGUAGE sql IMMUTABLE;

COMMENT ON FUNCTION audit_retention() IS
    'How long audit_logs rows are kept. Enforced by the audit_logs_append_only trigger: nothing younger than this can be deleted by anyone the trigger applies to.';

-- ── Archived before deleted ─────────────────────────────────────────────────
-- A purge that only deletes destroys the evidence it was keeping. Every purge
-- writes the rows it is about to remove to a file in the document store first
-- and records the file here; the purge refuses to delete anything it has not
-- archived (see internal/service/audit_retention.go).
CREATE TABLE audit_archives (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- The window this file covers. Two archives must never overlap, or a row
    -- would be counted twice and a gap between them would be invisible.
    covers_from  TIMESTAMPTZ NOT NULL,
    covers_to    TIMESTAMPTZ NOT NULL,
    row_count    BIGINT      NOT NULL,
    -- storage.Store key. The store is the same encrypted one the claim
    -- documents use.
    storage_key  TEXT        NOT NULL,
    size_bytes   BIGINT      NOT NULL,
    -- SHA-256 over the uncompressed JSONL. An archive nobody can verify is not
    -- evidence: this is what a later reader checks the file against.
    sha256       TEXT        NOT NULL,
    -- >=, not >: a batch holding a single row covers an instant, and that is a
    -- perfectly ordinary archive — the first purge of a quiet table is exactly
    -- this shape.
    CONSTRAINT audit_archives_window CHECK (covers_to >= covers_from)
);

CREATE INDEX audit_archives_covers_idx ON audit_archives (covers_to DESC);

COMMENT ON TABLE audit_archives IS
    'One row per purge: the off-table copy of the audit rows that were then deleted, with a checksum so the copy can be verified.';
