ALTER TABLE ta_profiles
    DROP COLUMN IF EXISTS citizen_id_key_version,
    DROP COLUMN IF EXISTS citizen_id_last4,
    DROP COLUMN IF EXISTS citizen_id_enc;
