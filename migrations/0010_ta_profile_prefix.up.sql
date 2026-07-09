-- Add prefix (คำนำหน้าชื่อ) to TA profiles.
--
-- The creditor form (แบบแจ้งข้อมูลเจ้าหนี้บุคลากร) requires the TA to circle
-- one of นาย/นาง/นางสาว next to their name. The overlay generator draws that
-- circle at a fixed x/y depending on which value is stored here, so this must
-- be one of the exact strings the generator knows about — enforced by CHECK.
--
-- The snapshot table gets the same column so history renders with the same
-- prefix as the round in which it was submitted.

ALTER TABLE ta_profiles
    ADD COLUMN IF NOT EXISTS prefix TEXT
        CHECK (prefix IS NULL OR prefix IN ('นาย','นาง','นางสาว'));

ALTER TABLE ta_profile_submissions
    ADD COLUMN IF NOT EXISTS prefix TEXT
        CHECK (prefix IS NULL OR prefix IN ('นาย','นาง','นางสาว'));
