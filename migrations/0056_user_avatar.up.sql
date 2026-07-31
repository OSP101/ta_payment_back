-- Profile picture (รูปโปรไฟล์).
--
-- Only the storage key lives in the row: the bytes go through the shared file
-- store, which is encrypted at rest when TA_DOCS_ENC_KEY is set. That keeps the
-- image behind the API's auth wall instead of on a guessable public path — a
-- face photo is personal data under PDPA just like the documents next to it.
--
-- avatar_updated_at is the cache-buster: the served URL carries it as ?v=, so a
-- replaced picture is never masked by a stale browser cache.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS avatar_key        TEXT,
    ADD COLUMN IF NOT EXISTS avatar_updated_at TIMESTAMPTZ;
