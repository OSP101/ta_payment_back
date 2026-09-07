DROP INDEX IF EXISTS ux_doc_progress_share_links_slug;
ALTER TABLE document_progress_share_links DROP COLUMN IF EXISTS slug;
