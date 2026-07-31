-- Let an admin restore a TA's download allowance without destroying the record
-- of the downloads already taken.
--
-- The obvious implementation of a reset is DELETE FROM ta_doc_downloads, and it
-- is wrong: those rows are the PII access trail (who pulled whose national ID
-- and bank details, and when). Deleting them would make the reset itself the
-- way to erase evidence — the opposite of why the table exists.
--
-- So rows are retired instead. The quota counts only rows with reset_at IS NULL;
-- a retired row still says the download happened, and now also says who
-- forgave it.
--
-- NULL on every existing row is correct: nothing has been reset yet.

ALTER TABLE ta_doc_downloads
    ADD COLUMN reset_at TIMESTAMPTZ,
    -- No ON DELETE: the admin who authorised the reset must remain attributable
    -- even after their account is closed, same reasoning as actor_id.
    ADD COLUMN reset_by UUID REFERENCES users(id);

-- The quota check now filters on reset_at, so it belongs in the index it uses.
DROP INDEX IF EXISTS ta_doc_downloads_user_round_idx;
CREATE INDEX ta_doc_downloads_active_idx
    ON ta_doc_downloads (user_id, round) WHERE reset_at IS NULL;
