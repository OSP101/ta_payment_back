-- 17/08/2026 — lets a TA with more than one ta_enrollments period (migration
-- 0094) pick which one their session is "viewing" — a TA who has since
-- advanced ป.ตรี -> โท still wants to be able to browse their old bachelor's
-- data separately from their current one, not just have staff-facing exports
-- get it right.
--
-- Lives on `sessions`, not a JWT claim: a claim is baked in at token-mint
-- time (see internal/auth/token.go), so switching mid-session would need a
-- full re-login. This column is read fresh per request by AccountGuard's
-- existing users<->sessions join (internal/handler/middleware.go), so a
-- change takes effect on the very next request and disappears naturally on
-- logout/session expiry along with the rest of the row — no JWT/login code
-- touched at all.
ALTER TABLE sessions
    ADD COLUMN selected_enrollment_id UUID REFERENCES ta_enrollments(id);

COMMENT ON COLUMN sessions.selected_enrollment_id IS
    'Which ta_enrollments period this TA session is currently "viewing" — a display filter only, never affects what a NEW ta_request_assignment attaches to (that always uses the truly active enrollment, see TARequestService.validateTA). NULL means unfiltered/no selection yet. Set via EnrollmentService.SetSessionScope, which validates the enrollment belongs to the session''s own user before writing.';
