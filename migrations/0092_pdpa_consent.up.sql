-- 16/08/2026 — PDPA consent record for the TA profile form.
--
-- ta_profiles collects a citizen ID (stored encrypted, migration 0076) and,
-- transiently within a single request, bank/PromptPay details that get
-- printed into the creditor-form PDF and forwarded to Finance for entry into
-- the university's ERP system. Before now, nothing recorded that a TA was
-- ever told this or agreed to it — see internal/service/pdpa_consent.go.
--
-- pdpa_consents
--   One row per (user, policy version), INSERT-only and never updated, so a
--   row is a genuine record of "this person clicked accept, at this time,
--   from this IP/device" rather than a mutable flag that could be edited
--   after the fact. version starts at 1 (a Go const, same pattern as
--   citizen_id_key_version / totp_key_version); bumping it later — if the
--   notice text materially changes — makes every existing TA re-consent
--   automatically, since HasPdpaConsent checks the CURRENT version only, with
--   no extra migration needed. UNIQUE(user_id, version) makes accepting the
--   same version twice (double-click, retry) a no-op instead of an error.
CREATE TABLE pdpa_consents (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    version      SMALLINT NOT NULL,
    consented_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ip           TEXT,
    user_agent   TEXT,
    UNIQUE (user_id, version)
);

COMMENT ON TABLE pdpa_consents IS
    'One row per (user, policy version) accepted. Insert-only proof of consent — see internal/service/pdpa_consent.go. UNIQUE(user_id, version) makes re-accepting the same version a no-op.';
COMMENT ON COLUMN pdpa_consents.version IS
    'The pdpaConsentVersion constant in effect when this row was written. Bumping the constant forces every user to re-consent, since gating checks for a row at the CURRENT version only.';
COMMENT ON COLUMN pdpa_consents.ip IS
    'c.IP() at the moment of acceptance — same capture convention as audit.Entry.IP elsewhere in this codebase.';
COMMENT ON COLUMN pdpa_consents.user_agent IS
    'c.Get("User-Agent") at the moment of acceptance — same capture convention as audit.Entry.UserAgent elsewhere in this codebase.';
