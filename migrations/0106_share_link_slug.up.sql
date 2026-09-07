-- 08/09/2026 — the share link is pasted into a LINE group and read off a
-- phone, and a 36-character UUID in the path made it long enough that staff
-- asked for something shorter:
--
--   /p/document-progress/2c684541-38a9-485f-b7cd-b98dae8fd78e   (before)
--   /p/document-progress/k7m2q9xr4wta                           (after)
--
-- The id stays the primary key and the audit identity; `slug` is only what
-- appears in the URL. 12 characters from a 32-symbol alphabet is 2^60
-- possibilities — the point of the token is that it cannot be guessed, and
-- shortening it must not quietly turn a public board into an enumerable one.
--
-- The alphabet omits 0/O and 1/l/i so a link read aloud or copied by hand off
-- a screenshot cannot land on a different term's board.
ALTER TABLE document_progress_share_links ADD COLUMN slug TEXT;

-- Existing links keep working at their old URL (the resolver still accepts a
-- UUID), but they get a short one too, so a link already posted can simply be
-- re-copied rather than revoked and reissued.
--
-- The subquery is correlated on l.id so random() is drawn per ROW; an
-- uncorrelated one would hand every row the same slug and trip the unique
-- index below.
UPDATE document_progress_share_links l
SET slug = (
    SELECT string_agg(
        substr('23456789abcdefghjkmnpqrstuvwxyz',
               (floor(random() * 31)::int) + 1, 1), '')
    FROM generate_series(1, 12)
    WHERE l.id IS NOT NULL
);

ALTER TABLE document_progress_share_links ALTER COLUMN slug SET NOT NULL;

CREATE UNIQUE INDEX ux_doc_progress_share_links_slug
    ON document_progress_share_links (slug);

COMMENT ON COLUMN document_progress_share_links.slug IS
    'Short unguessable URL token (12 chars, 31-symbol alphabet). The id remains the row identity; only this appears in a shared link.';
