DROP INDEX IF EXISTS admin_officers_active_user_uidx;
DROP INDEX IF EXISTS admin_officers_user_id_idx;
ALTER TABLE admin_officers DROP COLUMN IF EXISTS user_id;
