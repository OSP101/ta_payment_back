-- One e-mail, one ACTIVE account — not one account forever.
--
-- Staff close an account that was created wrong (wrong role, wrong name) and
-- need to create a fresh one for the same person. users.email was UNIQUE
-- across every row, so the closed account held the address hostage and the
-- create failed with "ข้อมูลนี้มีอยู่แล้วในระบบ".
--
-- Sign-in (password and KKU SSO) already resolves an e-mail to the ACTIVE
-- account only (UserService.FindByEmail), so the rule that actually matters
-- is "at most one active account per address". A closed account keeps its
-- address and its history; reopening it is refused while another active
-- account holds the same address (UserService.Activate says which one).
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_email_key;

CREATE UNIQUE INDEX IF NOT EXISTS ux_users_email_active
    ON users (email) WHERE is_active AND deleted_at IS NULL;

-- Lookups by address still need an index once the constraint's is gone.
CREATE INDEX IF NOT EXISTS ix_users_email ON users (email);
