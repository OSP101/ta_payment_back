-- 02/10/2026: backgrounds for the ประชาสัมพันธ์ cover maker.
--
-- Staff type the words of a notice onto a branded background in the composer
-- instead of making the picture in another program. The faculty's standard
-- background ships with the frontend; these are the extra ones staff upload, so
-- a new design never needs a code change. Every row is a 16:9 image at least
-- 1600×900 — the handler checks that before a row is written.
CREATE TABLE IF NOT EXISTS announcement_backgrounds (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    storage_key TEXT NOT NULL CHECK (storage_key LIKE 'announcements/%'),
    width       INTEGER NOT NULL CHECK (width >= 1600),
    height      INTEGER NOT NULL CHECK (height >= 900),
    -- Text colour that reads on this background: 'dark' for a pale picture,
    -- 'light' for a dark one. The maker starts from it; staff can still switch.
    text_tone   TEXT NOT NULL DEFAULT 'dark' CHECK (text_tone IN ('dark', 'light')),
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
