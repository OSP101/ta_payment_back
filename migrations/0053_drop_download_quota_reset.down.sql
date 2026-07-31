-- Re-adds the columns, empty. The reset history 0053 dropped is not recoverable
-- — nothing else recorded it — so every past reset stays counted.
ALTER TABLE ta_doc_downloads
    ADD COLUMN IF NOT EXISTS reset_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS reset_by UUID REFERENCES users(id);

DROP INDEX IF EXISTS ta_doc_downloads_user_round_idx;
CREATE INDEX ta_doc_downloads_active_idx
    ON ta_doc_downloads (user_id, round) WHERE reset_at IS NULL;
