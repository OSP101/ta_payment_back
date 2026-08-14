-- 14/08/2026 — staff want a link they can post to the department LINE group /
-- Facebook page so lecturers and TAs can check where the paperwork for a term
-- is without logging in, the same anonymous-by-design shape as public
-- announcements (see migration for `announcements.is_public`). Unlike an
-- announcement, this is not a message — it is a standing view onto ONE term's
-- document_progress board, so it gets its own table rather than being bolted
-- onto announcements.
--
-- id (not term_id) is the token in the URL, same reasoning as
-- announcements.id: an unguessable UUID that reveals nothing about how many
-- terms or links exist. A term has at most one LIVE link (the partial unique
-- index) — issuing a new one after revoking the last gives a fresh,
-- unrelated id, so an old link that leaked or was posted to the wrong group
-- can be cut off without touching the replacement.
CREATE TABLE document_progress_share_links (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id    UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    revoked_by UUID REFERENCES users(id)
);

CREATE UNIQUE INDEX ux_doc_progress_share_links_active_term
    ON document_progress_share_links (term_id) WHERE revoked_at IS NULL;
CREATE INDEX ix_doc_progress_share_links_term ON document_progress_share_links (term_id);

COMMENT ON TABLE document_progress_share_links IS
    'Staff-issued public links to one term''s document-progress board. id is the URL token; at most one row per term is live (revoked_at IS NULL) at a time.';
