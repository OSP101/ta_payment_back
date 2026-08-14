-- 14/08/2026 — single-device login + 15-minute idle auto-logout.
--
-- A JWT alone cannot be revoked before it expires (see main.go's original
-- Logout, which only cleared the cookie — a stolen token stayed valid for the
-- rest of its 12h lifetime). This table gives every issued token a server-side
-- row it can be checked against and killed early: logging in from a second
-- device revokes the first device's row, changing your password / being
-- deactivated / having your roles edited revokes every row, and a row whose
-- last_activity_at falls behind 15 minutes reads as expired without needing a
-- background job to notice — see internal/handler/middleware.go's AccountGuard.
--
-- id is the JWT's own "jti" claim (RegisteredClaims.ID) — the token carries
-- its session's identity, so validating a request means one join, not a
-- separate lookup table.
CREATE TABLE sessions (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Touched on every mutating request and on POST /auth/heartbeat — see
    -- AccountGuard. Never touched by GET/polling, so a tab left open on a
    -- read-only page cannot itself keep a session alive.
    last_activity_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Mirrors the JWT's own "exp" (config.JWTLifetime, 12h by default). The
    -- token is already rejected past this by jwt.ParseWithClaims, so nothing
    -- reads this column in the request path today — it exists so the cleanup
    -- sweep (scheduler.go) can find rows to delete without reimplementing JWT
    -- expiry math, and so a DB-only audit of "was this session still valid at
    -- time T" doesn't need the original token.
    expires_at       TIMESTAMPTZ NOT NULL,
    revoked_at       TIMESTAMPTZ,
    -- 'superseded' (new-device login), 'logout', 'idle_timeout',
    -- 'password_change', 'account_deactivated', 'roles_changed'. Free text,
    -- not an enum: this is read by developers debugging a "why was I logged
    -- out" report, not branched on in application logic.
    revoke_reason    TEXT,
    ip               TEXT,
    user_agent       TEXT
);

-- The query AccountGuard runs on every request: find this exact session by
-- its id (the JWT's jti), which is why id being the primary key is already
-- enough there. This index instead serves Login's "revoke every OTHER active
-- session for this user" step — without it that scan is a full table scan
-- once the table has more than a handful of rows.
CREATE INDEX ix_sessions_user_active ON sessions (user_id) WHERE revoked_at IS NULL;

-- The cleanup sweep's own lookup: rows that are done (revoked, or simply old)
-- and safe to delete.
CREATE INDEX ix_sessions_last_activity ON sessions (last_activity_at);

COMMENT ON TABLE sessions IS
    'One row per issued JWT (id = the token''s jti). Enforces single-device login (Login revokes every other active row for the user) and 15-minute idle timeout (AccountGuard checks last_activity_at). See internal/handler/middleware.go.';
