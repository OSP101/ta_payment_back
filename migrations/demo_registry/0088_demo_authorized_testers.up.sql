-- 15/08/2026 — who may enter the demo sandbox at all, and at what tier.
-- See docs/PLAN-demo-sandbox.md and internal/demo/tiers.go.
--
-- Before this, /api/demo/enter accepted any email and handed back every
-- seeded account (admin through TA) to pick from — fine while only staff
-- were trying the sandbox themselves, but wrong once real TAs and lecturers
-- get invited: a TA entering their own email had no reason to ever see the
-- staff or admin login cards, and nothing stopped them from logging into
-- one anyway (the demo password is the same for every seeded account, by
-- design — see seed.go's DemoPassword doc comment). This table is staff's
-- allowlist: an email not in it cannot claim a slot at all, and the tier
-- recorded here is re-checked on every single demo login (see
-- LoginHandler.Login), not just once at entry — so revoking someone here
-- takes effect on their very next request, not their next visit.
CREATE TABLE demo_authorized_testers (
    email      CITEXT PRIMARY KEY,
    -- 'staff' sees every seeded account; 'lecturer' sees lecturer + TA
    -- accounts; 'ta' sees TA accounts only — see tiers.go's tierAllowsRole.
    tier       TEXT NOT NULL CHECK (tier IN ('staff', 'lecturer', 'ta')),
    note       TEXT NOT NULL DEFAULT '',
    added_by   CITEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE demo_authorized_testers IS
    'BETA-only: allowlist of who may enter the demo sandbox and at what permission tier. Managed from /staff/settings. Safe to drop alongside demo_workspaces when the sandbox is retired.';
