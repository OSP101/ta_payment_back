-- Announcement composer upgrade: category, pin, cover image, scheduling,
-- expiry, and fanout tracking. Adds a matching soft-attention `announced_at`
-- so the background sweeper knows which scheduled rows still need fanout.

ALTER TABLE announcements
    ADD COLUMN IF NOT EXISTS category         TEXT NOT NULL DEFAULT 'info',
    ADD COLUMN IF NOT EXISTS pinned           BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS cover_image_key  TEXT,
    ADD COLUMN IF NOT EXISTS expires_at       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS announced_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS updated_by       UUID REFERENCES users(id);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'announcements_category_check'
    ) THEN
        ALTER TABLE announcements
            ADD CONSTRAINT announcements_category_check
            CHECK (category IN ('info','news','warning','urgent','event'));
    END IF;
END$$;

-- Speeds up the audience+active filter used every time an authenticated user
-- opens their notification bell or the announcements page.
CREATE INDEX IF NOT EXISTS announcements_pub_idx
    ON announcements (published_at DESC NULLS LAST)
    WHERE published_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS announcements_pending_fanout_idx
    ON announcements (published_at)
    WHERE announced_at IS NULL AND published_at IS NOT NULL;
