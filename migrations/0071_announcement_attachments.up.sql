-- 06/08/2026: an announcement can carry files, the way a post on a group page
-- does — several photos, a clip, the PDF of the official notice — instead of
-- the single cover image it had.
--
-- The cover image stays as its own column: it is the thumbnail the feed shows,
-- a different job from "here are the documents attached to this notice".
CREATE TABLE IF NOT EXISTS announcement_attachments (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    announcement_id UUID NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
    -- How the reader should see it. Decided at upload from the sniffed bytes,
    -- never from the filename: 'image' lays out in a gallery, 'video' gets a
    -- player, 'file' is a download row.
    kind            TEXT NOT NULL CHECK (kind IN ('image', 'video', 'file')),
    storage_key     TEXT NOT NULL,
    -- The name the uploader saw. Shown for downloads, never used to build a
    -- path — storage_key is the only thing that touches the store.
    filename        TEXT NOT NULL,
    mime            TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    -- Composer order. Photos in a gallery read as a sequence, so the order the
    -- officer arranged them in has to survive.
    sort_order      INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS announcement_attachments_ann_idx
    ON announcement_attachments (announcement_id, sort_order);

-- Serving a file to an anonymous reader means answering "may this key be seen
-- without an account". That is a lookup by KEY, and it has to cover the cover
-- image as well — the public page shipped earlier renders one through an
-- authenticated route, so every shared announcement with a picture showed a
-- broken image to exactly the people it was shared with.
CREATE INDEX IF NOT EXISTS announcement_attachments_key_idx
    ON announcement_attachments (storage_key);
CREATE INDEX IF NOT EXISTS announcements_cover_key_idx
    ON announcements (cover_image_key) WHERE cover_image_key IS NOT NULL;
