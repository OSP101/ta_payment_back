-- 09/08/2026 — deliberate, partial reversal of migration 0047's PDPA scrub.
--
-- The ปะหน้าจ่ายตรง transfer-cover document (the second document from the
-- staff interview that drove this whole plan) needs the TA's citizen ID
-- number printed on it. 0047 removed the plaintext column outright rather
-- than half-heartedly clearing it later, on the reasoning that nothing should
-- be stored that only exists to be drawn onto a PDF and thrown away.
--
-- That reasoning no longer applies to JUST this one field: staff confirmed
-- they need it retrievable, on the explicit condition that it is encrypted so
-- nothing but this system can read it back. So this migration undoes 0047
-- for citizen_id ONLY — bank details, branch/account info, and the signature
-- stay gone. Bringing those back was never asked for and the transfer-cover
-- sheet does not print them.
--
-- Two columns, not one:
--   citizen_id_enc    — XChaCha20-Poly1305 ciphertext (internal/pii), key
--                       PII_ENC_KEY, DELIBERATELY SEPARATE from the document
--                       storage key so one leaking does not leak the other.
--                       Layout: [24-byte nonce][ciphertext+16-byte tag].
--                       AAD is the owning user's id, so a ciphertext copied
--                       onto a different row fails to decrypt.
--   citizen_id_last4  — plaintext, e.g. "0123". Lets every screen that only
--                       needs to show "on file, ending ...0123" skip
--                       decrypting on every page load; only the one function
--                       that builds the transfer-cover document (or a staff
--                       member deliberately choosing to reveal the full
--                       number) ever calls Open. Derived from the SAME
--                       plaintext as citizen_id_enc at write time — never
--                       accepted as separate client input, or the two could
--                       silently disagree.
-- citizen_id_key_version tracks which key encrypted a given row, so PII_ENC_KEY
-- can be rotated later without a flag day that breaks every existing value.
ALTER TABLE ta_profiles
    ADD COLUMN IF NOT EXISTS citizen_id_enc BYTEA,
    ADD COLUMN IF NOT EXISTS citizen_id_last4 TEXT,
    ADD COLUMN IF NOT EXISTS citizen_id_key_version SMALLINT;

COMMENT ON COLUMN ta_profiles.citizen_id_enc IS
    'XChaCha20-Poly1305 ciphertext of the 13-digit citizen ID: [24B nonce][ciphertext+16B tag]. Key = PII_ENC_KEY (internal/pii), AAD = user_id. Never returned by any API — see GetProfile.';
COMMENT ON COLUMN ta_profiles.citizen_id_last4 IS
    'Last 4 digits, plaintext, for on-screen display without decrypting. Always derived from the same plaintext as citizen_id_enc at write time.';
COMMENT ON COLUMN ta_profiles.citizen_id_key_version IS
    'Which PII_ENC_KEY version encrypted citizen_id_enc, for key rotation.';
