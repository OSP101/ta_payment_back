-- 06/10/2026 — second deliberate, partial reversal of migration 0047.
--
-- The finance office asked for the "Template-Suppliers" sheet: one row per TA
-- who is new that term, imported into the university ERP as a supplier
-- (POZ_SUPPLIERS_INT) with a bank account (IBY_TEMP_EXT_BANK_ACCTS). Its
-- columns need the bank name, branch and account number, and a postal address,
-- none of which this system kept: 0047 dropped the bank columns, and the
-- address was never asked for.
--
-- Same terms as 0076 brought the citizen ID back on: encrypted with
-- PII_ENC_KEY (internal/pii), never returned by any API, read back only by the
-- export that prints it, and every read is audited. One sealed JSON value
-- rather than seven ciphertext columns — the fields are always written and
-- read together, and one nonce/tag per row is one thing to rotate.
--
--   payee_enc         — XChaCha20-Poly1305 ciphertext of the JSON
--                       {bank_name, bank_branch, branch_code, account_no,
--                        account_name, address, postal_code}.
--                       Layout: [24-byte nonce][ciphertext+16-byte tag].
--                       AAD is user_id || "payee", so it cannot be swapped
--                       with another row, nor with this row's citizen_id_enc.
--   payee_key_version — which PII_ENC_KEY version sealed it (key rotation).
--
-- TAs approved before this migration have no value; it fills in the next time
-- they send the profile form.
ALTER TABLE ta_profiles
    ADD COLUMN IF NOT EXISTS payee_enc BYTEA,
    ADD COLUMN IF NOT EXISTS payee_key_version SMALLINT;

COMMENT ON COLUMN ta_profiles.payee_enc IS
    'XChaCha20-Poly1305 ciphertext of the TA bank account + postal address JSON: [24B nonce][ciphertext+16B tag]. Key = PII_ENC_KEY, AAD = user_id || ''payee''. Never returned by any API — read only by the Suppliers export.';
COMMENT ON COLUMN ta_profiles.payee_key_version IS
    'Which PII_ENC_KEY version encrypted payee_enc, for key rotation.';
