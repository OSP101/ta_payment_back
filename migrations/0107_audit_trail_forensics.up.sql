-- 08/09/2026 — the audit trail could not be used to follow anything.
--
-- Measured on the live table before this migration (407 rows):
--
--     rows | actor | actor_role |  ip | user_agent | before | after
--      407 |   407 |          0 |   0 |        141 |      0 |   136
--
-- Every row says WHO and WHAT. Almost none say from WHERE, none say in what
-- ROLE, none say what the value WAS before it changed, and nothing at all ties
-- a row to the HTTP request that produced it or to the stdout line the same
-- request logged. An investigation ("who changed this TA's hours, from what,
-- on whose session, from which machine") had no query that could answer it.
--
-- actor_role was declared in 0001 and never once written. ip was written by 12
-- of 122 call sites and still landed NULL every time — see main.go: fiber's
-- ProxyHeader was set unconditionally with EnableIPValidation off, and fiber
-- then returns the X-Forwarded-For header "even if it is empty or invalid"
-- (their comment) rather than falling back to the socket address.

-- ── Correlation ─────────────────────────────────────────────────────────────
-- request_id is minted per HTTP request, returned to the client in
-- X-Request-Id, and stamped on the structured stdout log line AND on every
-- audit row the request writes. It is the join key between the three, and the
-- one thing a support ticket can quote.
ALTER TABLE audit_logs ADD COLUMN request_id UUID;

-- Which login session acted. users.id says who the account is; this says which
-- of that account's live sessions — the difference between "the lecturer did
-- it" and "someone holding a token the lecturer issued in March did it".
ALTER TABLE audit_logs ADD COLUMN session_id UUID;

-- ── The request itself ──────────────────────────────────────────────────────
-- Kept denormalised on the row on purpose: an audit trail has to stay readable
-- when the code that produced it has been rewritten, and "POST /api/v1/…"
-- survives a handler being renamed in a way that an action string does not.
ALTER TABLE audit_logs ADD COLUMN method TEXT;
ALTER TABLE audit_logs ADD COLUMN path   TEXT;
ALTER TABLE audit_logs ADD COLUMN status INT;

COMMENT ON COLUMN audit_logs.request_id IS
    'Correlates this row with the X-Request-Id returned to the client and with the structured application log for the same request.';
COMMENT ON COLUMN audit_logs.session_id IS
    'sessions.id (the JWT jti) the actor was holding. No FK: sessions are swept on expiry and the audit row must outlive them.';

-- ── Indexes for the questions an investigation actually asks ────────────────
-- "everything this request did"
CREATE INDEX audit_request_idx ON audit_logs (request_id) WHERE request_id IS NOT NULL;
-- "everything this session did, newest first"
CREATE INDEX audit_session_idx ON audit_logs (session_id, at DESC) WHERE session_id IS NOT NULL;
-- "every login failure this week" — action was previously only reachable by a
-- full scan, which is why the UI could offer no action filter.
CREATE INDEX audit_action_idx  ON audit_logs (action, at DESC);
-- "everything from this address" — the point of recording it.
CREATE INDEX audit_ip_idx      ON audit_logs (ip, at DESC) WHERE ip IS NOT NULL;

-- ── Append-only ─────────────────────────────────────────────────────────────
-- A trail that the application role can edit is not evidence. The app inserts
-- and reads; nothing in the product has any reason to update or delete a row,
-- so make the database refuse rather than trusting that no future handler will.
--
-- A trigger rather than only a REVOKE: the app connects as the schema owner
-- here, and an owner's privileges cannot be revoked from themselves. The
-- trigger binds the owner too. A superuser can still drop it — that is the
-- honest limit of in-database tamper-proofing, and the reason retention beyond
-- this belongs in an off-box copy (see the phase 4 note in internal/audit).
CREATE OR REPLACE FUNCTION audit_logs_append_only() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs is append-only: % is not permitted', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_logs_no_update
    BEFORE UPDATE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_append_only();

CREATE TRIGGER audit_logs_no_delete
    BEFORE DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_append_only();
