-- Cap how many times one TA's approved document bundle can be downloaded.
--
-- The bundle carries a national ID, a bank account number and a signature.
-- Password-gating the download proves WHO pulled it; it does not limit HOW MANY
-- copies leave the system. This table does, and doubles as the PII access trail
-- for those copies (audit_log records the event; this one is what the rule is
-- enforced against, inside the same transaction that serves the bytes).
--
-- Scoped to (TA, round), not to (TA, officer):
--   * The thing being rationed is copies of one person's documents, so counting
--     per officer would let three officers take six copies between them.
--   * Tying it to `round` gives the only recovery path that does not need a DBA:
--     when a TA is asked to resubmit, that is a new document set and a fresh
--     allowance. Without it, a corrupted download would strand the finance
--     handoff permanently.
--
-- No unique constraint on (user_id, round): the row IS the counter, so several
-- are expected. The index carries the COUNT(*) the quota check runs.

CREATE TABLE ta_doc_downloads (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The TA whose documents were downloaded.
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Submission round the downloaded documents belonged to.
    round         INT  NOT NULL,
    -- The officer who pulled them. ON DELETE is deliberately absent: a
    -- deactivated officer's access record must not disappear with them.
    actor_id      UUID NOT NULL REFERENCES users(id),
    downloaded_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX ta_doc_downloads_user_round_idx
    ON ta_doc_downloads (user_id, round);
