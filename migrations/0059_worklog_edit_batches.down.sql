-- Files first: the FK cascades, but dropping in dependency order keeps the
-- rollback readable and avoids relying on cascade behaviour that a future
-- schema change could remove.
DROP TABLE IF EXISTS worklog_edit_files;
DROP TABLE IF EXISTS worklog_edit_batches;
