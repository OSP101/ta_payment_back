-- Dropping the table discards the download history along with the quota. That
-- is the honest consequence of rolling back: the counter and the access trail
-- are the same rows, and there is nowhere else to keep them.
DROP TABLE IF EXISTS ta_doc_downloads;
