-- Fails if two rows now share an address; close or rename one first.
DROP INDEX IF EXISTS ix_users_email;
DROP INDEX IF EXISTS ux_users_email_active;
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE (email);
