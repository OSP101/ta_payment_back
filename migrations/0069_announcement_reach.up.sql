-- 06/08/2026: the ประชาสัมพันธ์ feature grows two more ways to reach people.
-- Until now an announcement could only reach accounts that already exist and
-- hold one of the audience roles — there was no way to mail an address that is
-- not a user, and nothing to share outside the system at all.

-- ---------------------------------------------------------------------------
-- 1. Sharing outside the system
-- ---------------------------------------------------------------------------
-- A shared Facebook/LINE link is worthless if the page behind it demands a
-- login, so staff may open ONE announcement at a time to anonymous readers.
-- Opt-in per row and defaulting to FALSE: announcements carry internal notices
-- (pay rates, names, deadlines), and a flag that defaulted to public would have
-- exposed every existing row the moment this migration ran.
ALTER TABLE announcements
    ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN announcements.is_public IS
    'TRUE = readable without logging in, via the public share link. Staff opt in per announcement; the public view still respects published_at/expires_at.';

-- ---------------------------------------------------------------------------
-- 2. Named email recipients
-- ---------------------------------------------------------------------------
-- One row per address an announcement should additionally reach: a user picked
-- from the system, or an address typed by hand (a guest lecturer, a faculty
-- mailing list, someone who has no account yet).
--
-- This table is a LEDGER, not a settings list. It records what was actually
-- delivered, which is what makes re-sending safe: a second send only touches
-- rows still pending. Without it, "ส่งอีเมลอีกครั้ง" would mail everyone twice.
CREATE TABLE IF NOT EXISTS announcement_recipients (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    announcement_id UUID NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
    email           TEXT NOT NULL,
    -- Set when the recipient was picked from the user list; NULL for a typed
    -- address. Kept so the composer can show a name instead of a bare address,
    -- and so a deleted account does not delete the delivery record.
    user_id         UUID REFERENCES users(id) ON DELETE SET NULL,
    -- pending = queued, sent = mail accepted, skipped = already covered by the
    -- role audience so we deliberately did not send twice, failed = the mailer
    -- refused it (address kept so staff can correct and retry).
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'sent', 'skipped', 'failed')),
    sent_at         TIMESTAMPTZ,
    error           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One row per address per announcement. Case-insensitive because a human
-- typing the same address twice with different capitalisation means one
-- person, and this index is what stops them being mailed twice.
CREATE UNIQUE INDEX IF NOT EXISTS ux_announcement_recipients_email
    ON announcement_recipients (announcement_id, lower(email));

-- The send loop asks for exactly this set.
CREATE INDEX IF NOT EXISTS announcement_recipients_pending_idx
    ON announcement_recipients (announcement_id) WHERE status = 'pending';
