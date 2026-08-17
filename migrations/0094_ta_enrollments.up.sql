-- 17/08/2026 — TA education-level history (enrollment periods).
--
-- A university-issued student_id and study_level are NOT permanent facts
-- about a person the way email is: this university reissues a NEW student_id
-- every time a TA advances to a new degree level (ป.ตรี -> โท -> เอก), while
-- the TA keeps using the SAME university email, and the same login/account,
-- for life. Until now users.student_id/study_level were bare mutable columns
-- on `users`, silently overwritten by the TA's own profile form
-- (DocsService.UpsertProfile) with no history and no record of who changed
-- what when — a level change today would retroactively make every past
-- export/admin view show the NEW id/level for work done under the OLD one.
--
-- This table is the source of truth for "what student_id/level applied
-- when". users.student_id/study_level (untouched by this migration) remain a
-- denormalized "current" projection, kept in sync by
-- EnrollmentService.RecordTransition — see internal/service/enrollment.go.
CREATE TABLE ta_enrollments (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    student_id  TEXT NOT NULL,
    study_level study_level NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- NULL = this is the TA's current, active period. Set the instant a
    -- newer period is recorded for the same user, in the same transaction
    -- that inserts the new row (RecordTransition) — there is deliberately no
    -- trigger; see that function's doc comment for why.
    ended_at    TIMESTAMPTZ,
    -- Staff/admin who entered this period. NULL only for rows created by the
    -- one-time backfill below — nobody "recorded" those, they're inferred
    -- from pre-existing users.student_id.
    created_by  UUID REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    note        TEXT
);

CREATE INDEX ta_enrollments_user_idx ON ta_enrollments (user_id, started_at DESC);

-- At most one active (ended_at IS NULL) period per user, same partial-unique
-- shape as migration 0066's ux_academic_terms_single_active. Zero active
-- periods stays legal (brand new account, no profile submitted yet).
CREATE UNIQUE INDEX ux_ta_enrollments_one_active
    ON ta_enrollments (user_id) WHERE ended_at IS NULL;

COMMENT ON TABLE ta_enrollments IS
    'Append-only history of a TA''s (student_id, study_level) periods. Staff-authored transitions only (EnrollmentService.RecordTransition). The TA''s own first-ever profile submission (DocsService.UpsertProfile) also creates a row here when the user has none yet — see UpsertProfile''s comment.';

-- One-time backfill: every existing user with a non-null student_id becomes
-- an "active" first enrollment period. No history to reconstruct — there are
-- no real level-transition cases yet, so one row per user is exactly
-- correct, not a simplification of something messier.
INSERT INTO ta_enrollments (user_id, student_id, study_level, started_at, created_by, note)
SELECT id, student_id, COALESCE(study_level, 'undergrad'), created_at, NULL,
       'backfilled from users.student_id at 0094 migration time'
FROM users
WHERE student_id IS NOT NULL AND deleted_at IS NULL;
