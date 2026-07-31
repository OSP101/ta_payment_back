-- Reverting drops the bulk-download records along with the flag. That is the
-- honest outcome: without the flag there is no way to hold a row that must not
-- count, and leaving them would silently charge every bulk pull against the
-- quota — locking TAs out of individual re-download.
DELETE FROM ta_doc_downloads WHERE NOT counts_toward_quota;

DROP INDEX IF EXISTS ta_doc_downloads_quota_idx;

ALTER TABLE ta_doc_downloads
    DROP COLUMN IF EXISTS counts_toward_quota;
