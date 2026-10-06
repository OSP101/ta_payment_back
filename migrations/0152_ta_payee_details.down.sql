ALTER TABLE ta_profiles
    DROP COLUMN IF EXISTS payee_enc,
    DROP COLUMN IF EXISTS payee_key_version;
