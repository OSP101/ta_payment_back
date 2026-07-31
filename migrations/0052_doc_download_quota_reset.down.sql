-- Rolling back loses the record of which downloads were forgiven and by whom.
-- The download rows themselves survive; every retired one silently counts again,
-- so a TA whose quota was reset goes back to being out of allowance.
DROP INDEX IF EXISTS ta_doc_downloads_active_idx;
CREATE INDEX ta_doc_downloads_user_round_idx
    ON ta_doc_downloads (user_id, round);

ALTER TABLE ta_doc_downloads
    DROP COLUMN IF EXISTS reset_by,
    DROP COLUMN IF EXISTS reset_at;
