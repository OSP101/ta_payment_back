ALTER TABLE users
    DROP COLUMN IF EXISTS avatar_key,
    DROP COLUMN IF EXISTS avatar_updated_at;
