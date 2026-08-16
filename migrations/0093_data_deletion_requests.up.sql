-- 16/08/2026 — self-service PDPA data-deletion requests.
--
-- A TA can ask to have their personal data deleted (POST /me/data-deletion-request);
-- an admin reviews it (POST /staff/data-deletion-requests/:id/review) and either
-- rejects with a reason, or approves — which runs DataDeletionService.ReviewDeletion
-- and does one of two things depending on whether the TA has any approved/paid
-- worklog or appointment history (HasPaymentHistory):
--
--   partial (payment history exists): financial records (citizen ID, worklogs,
--   appointment orders, audit trail) are RETAINED — Thai accounting/tax retention
--   requirements apply to those, not something a data-subject request can override.
--   Everything else (2FA, avatar, sessions, document blobs) is still scrubbed and
--   the account deactivated.
--
--   full (no payment history at all): the same scrub PLUS ta_profiles.citizen_id_enc/
--   citizen_id_last4/citizen_id_key_version are cleared and users.deleted_at is set —
--   the first real writer of that column. Every other query in this codebase already
--   filters `WHERE deleted_at IS NULL` (see internal/service/user.go's Get/List/
--   FindByEmail and middleware.go's AccountGuard) but nothing has ever set it until
--   now; it existed since migration 0001 waiting for exactly this use.
--
-- Which branch ran is recorded in the audit_logs "user.pdpa_erasure" Note, not just
-- inferred from column state after the fact — see ReviewDeletion's own doc comment.
CREATE TABLE data_deletion_requests (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reason       TEXT,
    status       TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','approved','rejected')),
    requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_at  TIMESTAMPTZ,
    reviewed_by  UUID REFERENCES users(id),
    review_note  TEXT,
    executed_at  TIMESTAMPTZ
);

COMMENT ON TABLE data_deletion_requests IS
    'Self-service PDPA erasure requests. See internal/service/data_deletion.go for the review/approval workflow and the partial-vs-full retention branch.';
COMMENT ON COLUMN data_deletion_requests.executed_at IS
    'When ReviewDeletion actually ran the scrub, distinct from reviewed_at (the decision timestamp) — the two are the same instant today since approval executes synchronously, but kept separate in case that ever changes.';

-- One live (pending) request per user — a second submission while one is already
-- pending is a clean 23505 unique-violation, which internal/handler/middleware.go's
-- ErrorHandler already turns into a friendly Thai 409 with no special-case code.
CREATE UNIQUE INDEX ix_data_deletion_requests_one_pending
    ON data_deletion_requests (user_id) WHERE status = 'pending';

-- The staff review queue's own lookup: list pending requests newest-first.
CREATE INDEX ix_data_deletion_requests_status ON data_deletion_requests (status, requested_at DESC);
