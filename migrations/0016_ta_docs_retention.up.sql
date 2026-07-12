-- TA document retention policy.
--
-- Motivation:
--   After a staff officer approves a TA's submitted docs, the audit copy is
--   downloaded as a ZIP and the physical file no longer needs to live on
--   disk. The row itself must survive for audit trail, but the blob is
--   scrubbed after 7 days by a background sweep. This migration adds the
--   fields the sweep needs and the batch-id used to group per-file rejects
--   from a single officer action.
--
-- Semantics vs superseded_at:
--   - superseded_at: TA uploaded a replacement doc (set in Upload).
--   - expires_at / file_deleted_at: retention policy on approved originals.
--   The two flows are independent — a doc can be both superseded (history)
--   and later purged from disk.

ALTER TABLE ta_documents
    ADD COLUMN IF NOT EXISTS expires_at       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS file_deleted_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS reject_batch_id  UUID;

-- Cheap scan for the retention sweep.
CREATE INDEX IF NOT EXISTS ta_documents_expiry_idx
    ON ta_documents (expires_at)
    WHERE expires_at IS NOT NULL AND file_deleted_at IS NULL;
