-- Link admin_officers to a real users row instead of free-typed names. Staff
-- used to retype a name + prefix from scratch for every officer, which drifted
-- from that person's actual account (typos, later name changes never
-- reflected here). Selecting an existing account keeps the two in sync.
--
-- full_name/academic_prefix stay as columns (existing raw SQL in
-- signer_authority.go/certifier.go still reads them for document generation),
-- but they are now written FROM the linked user at save time, never typed
-- directly — see AdminOfficerService.Upsert.
ALTER TABLE admin_officers ADD COLUMN user_id UUID REFERENCES users(id);

-- Backfill: every existing officer's full_name matches exactly one active
-- user's first_name || ' ' || last_name in this install. Best-effort — a row
-- that doesn't match stays unlinked and must be re-picked once via "แก้ไข".
UPDATE admin_officers ao
SET user_id = u.id
FROM users u
WHERE ao.user_id IS NULL
  AND u.deleted_at IS NULL
  AND u.first_name || ' ' || u.last_name = ao.full_name;

CREATE INDEX admin_officers_user_id_idx ON admin_officers (user_id);

-- One active seat per person — a person can hold at most one active
-- admin_officers row at a time (they can still appear in inactive history).
CREATE UNIQUE INDEX admin_officers_active_user_uidx
    ON admin_officers (user_id) WHERE is_active AND user_id IS NOT NULL;
