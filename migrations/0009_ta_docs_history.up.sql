-- TA document + profile submission history.
--
-- Motivation:
--   Sensitive TA onboarding docs (national ID copy, bank book, creditor form)
--   are submitted once, then reviewed by staff who may reject the batch with
--   notes. Historically we overwrote each doc in place, losing the previous
--   version + rejection reason as soon as the TA re-submitted. Staff need to
--   be able to look back at prior submissions even after final approval, and
--   auditors need a trail of what was approved vs what was replaced.
--
-- Changes:
--   1) ta_documents gains superseded_at / round so newer uploads of the same
--      kind supersede older ones without deleting them; the "current" doc for
--      a (user, kind) is the row with superseded_at IS NULL.
--   2) ta_profile_submissions stores one immutable snapshot per profile
--      submission round with the review outcome and reject reason for that
--      round.
--   3) ta_profiles gains current_round to make it obvious which submission
--      row on ta_profile_submissions is the live one.

ALTER TABLE ta_documents
    ADD COLUMN IF NOT EXISTS superseded_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS superseded_by UUID REFERENCES ta_documents(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS round INT NOT NULL DEFAULT 1;

-- Only one current row per (user, kind).
CREATE UNIQUE INDEX IF NOT EXISTS ta_documents_current_uidx
    ON ta_documents (user_id, kind)
    WHERE superseded_at IS NULL;

ALTER TABLE ta_profiles
    ADD COLUMN IF NOT EXISTS current_round INT NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS ta_profile_submissions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    round         INT NOT NULL,
    -- Immutable snapshot of the profile data at submit-time. National ID is
    -- kept as-plaintext here to match ta_profiles; deployments that need
    -- column-level encryption can enable pgcrypto and re-column later.
    national_id   TEXT,
    bank_name     TEXT,
    bank_branch   TEXT,
    branch_code   TEXT,
    account_no    TEXT,
    account_name  TEXT,
    signature_svg TEXT,
    submitted_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status        doc_status NOT NULL DEFAULT 'submitted',
    reviewed_at   TIMESTAMPTZ,
    reviewed_by   UUID REFERENCES users(id),
    reject_reason TEXT,
    UNIQUE (user_id, round)
);
CREATE INDEX IF NOT EXISTS ta_profile_submissions_user_idx
    ON ta_profile_submissions (user_id, submitted_at DESC);
