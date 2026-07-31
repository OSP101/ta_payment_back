-- Recreates the columns empty. The data is gone for good — that is the point
-- of 0047, not an accident of it.

ALTER TABLE ta_profiles
    ADD COLUMN IF NOT EXISTS national_id TEXT,
    ADD COLUMN IF NOT EXISTS national_id_provided_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS bank_name TEXT,
    ADD COLUMN IF NOT EXISTS bank_branch TEXT,
    ADD COLUMN IF NOT EXISTS branch_code TEXT,
    ADD COLUMN IF NOT EXISTS account_no TEXT,
    ADD COLUMN IF NOT EXISTS account_name TEXT,
    ADD COLUMN IF NOT EXISTS signature_svg TEXT,
    ADD COLUMN IF NOT EXISTS signature_png_b64 TEXT;

ALTER TABLE ta_profile_submissions
    ADD COLUMN IF NOT EXISTS national_id TEXT,
    ADD COLUMN IF NOT EXISTS bank_name TEXT,
    ADD COLUMN IF NOT EXISTS bank_branch TEXT,
    ADD COLUMN IF NOT EXISTS branch_code TEXT,
    ADD COLUMN IF NOT EXISTS account_no TEXT,
    ADD COLUMN IF NOT EXISTS account_name TEXT,
    ADD COLUMN IF NOT EXISTS signature_svg TEXT;
