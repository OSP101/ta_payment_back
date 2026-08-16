DROP TABLE IF EXISTS mfa_challenges;
DROP TABLE IF EXISTS mfa_recovery_codes;

ALTER TABLE users
    DROP COLUMN IF EXISTS totp_secret_enc,
    DROP COLUMN IF EXISTS totp_pending_secret_enc,
    DROP COLUMN IF EXISTS totp_key_version,
    DROP COLUMN IF EXISTS totp_enabled_at,
    DROP COLUMN IF EXISTS totp_last_step;
