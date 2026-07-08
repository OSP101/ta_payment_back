-- Add prefix (title) column and must-change-password flag to users.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS title TEXT,
    ADD COLUMN IF NOT EXISTS must_change_password BOOLEAN NOT NULL DEFAULT FALSE;
