package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IdleTimeout is how long a session may go without a touch (see Touch) before
// AccountGuard treats it as expired. A constant, not config: the 15-minute
// figure is a product decision (see docs), not an environment-specific tuning
// knob, and hardcoding it means every reader of AccountGuard sees the real
// number instead of chasing an env var.
const IdleTimeout = 15 * time.Minute

// SessionService backs single-device login (a new login revokes every other
// active session for the user) and the 15-minute idle timeout. See migration
// 0085 and internal/handler/middleware.go's AccountGuard, which is the actual
// enforcement point — this service only does the reads/writes it needs.
type SessionService struct {
	pool *pgxpool.Pool
}

// CreateAndSupersede revokes every other active session for userID (reason
// "superseded" — this is the single-device-login rule: the new login wins,
// the old device is kicked) and inserts a new one. Both happen in one
// transaction so a crash between them can never leave two sessions active.
//
// ttl should be config.JWTLifetime. expires_at is not read on the request
// path — the JWT's own exp claim already enforces that — only by Cleanup,
// which needs a cutoff to find rows safe to delete.
func (s *SessionService) CreateAndSupersede(ctx context.Context, userID uuid.UUID, ttl time.Duration, ip, userAgent string) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET revoked_at = NOW(), revoke_reason = 'superseded'
		 WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return uuid.Nil, err
	}

	id := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO sessions (id, user_id, expires_at, ip, user_agent)
		 VALUES ($1, $2, NOW() + make_interval(secs => $3), $4, $5)`,
		id, userID, ttl.Seconds(), ip, userAgent); err != nil {
		return uuid.Nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// Touch extends a session's idle window — called for mutating requests and
// POST /auth/heartbeat. See AccountGuard, which does the actual
// active/idle/revoked decision as part of its own combined query (checking
// that alongside the account's is_active/must_change_password in one round
// trip); this is only the write half, used once that decision comes back
// "still good".
func (s *SessionService) Touch(ctx context.Context, sessionID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET last_activity_at = NOW() WHERE id = $1`, sessionID)
	return err
}

// MarkIdle revokes a session that AccountGuard found past IdleTimeout. Called
// from there rather than left for the cleanup sweep so a second request on
// the same dead session — a background tab's next poll, say — sees a
// consistently revoked row instead of a live one that merely reads as idle.
func (s *SessionService) MarkIdle(ctx context.Context, sessionID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = NOW(), revoke_reason = 'idle_timeout'
		 WHERE id = $1 AND revoked_at IS NULL`, sessionID)
	return err
}

// Revoke ends one session — used by Logout. Revoking an already-revoked or
// unknown id is not an error: logging out twice, or logging out after the
// session was already superseded elsewhere, should both just succeed.
func (s *SessionService) Revoke(ctx context.Context, sessionID uuid.UUID, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = NOW(), revoke_reason = $2
		 WHERE id = $1 AND revoked_at IS NULL`, sessionID, reason)
	return err
}

// RevokeAllForUser ends every active session belonging to userID — used
// whenever something makes the account's existing tokens untrustworthy:
// changing the password, being deactivated, or having roles edited. Under
// single-device login there is at most one row to revoke, but the query does
// not assume that; it is correct even if that invariant is ever relaxed.
func (s *SessionService) RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = NOW(), revoke_reason = $2
		 WHERE user_id = $1 AND revoked_at IS NULL`, userID, reason)
	return err
}

// Cleanup deletes session rows that can no longer matter to anything: revoked
// (for any reason) or past their expires_at, and last touched more than a
// week ago. The week of slack is not a correctness requirement — a revoked
// row is already inert the instant revoked_at is set — it exists so a recent
// "why was I logged out" support question can still be answered by reading
// the row instead of the audit log. Called from the scheduler's daily sweep.
func (s *SessionService) Cleanup(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM sessions
		 WHERE (revoked_at IS NOT NULL OR expires_at < NOW())
		   AND last_activity_at < NOW() - INTERVAL '7 days'`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
