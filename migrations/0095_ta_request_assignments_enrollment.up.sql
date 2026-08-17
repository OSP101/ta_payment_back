-- 17/08/2026 — snapshot which ta_enrollments period (migration 0094) backed
-- each TA request assignment, the same way .level has always been
-- snapshotted (0001_init) rather than re-derived from the live users row.
--
-- enrollment_id is a traceability FK (which exact period produced this
-- assignment); student_id_snapshot is a denormalized copy of that period's
-- student_id so the handful of read sites that already SELECT a.level can
-- add student_id at zero extra JOIN cost, instead of every one of them
-- needing a new JOIN to ta_enrollments. Both are nullable and populated only
-- going forward by TARequestService.validateTA/Create — pre-existing
-- assignment rows have no created_at column to match against a point in
-- time, so they are left NULL rather than guessed at (see the best-effort
-- backfill below, which only fills in the unambiguous case).
ALTER TABLE ta_request_assignments
    ADD COLUMN enrollment_id UUID REFERENCES ta_enrollments(id),
    ADD COLUMN student_id_snapshot TEXT;

COMMENT ON COLUMN ta_request_assignments.enrollment_id IS
    'The ta_enrollments row active for this TA when this assignment was created. Snapshot, never updated after insert — same rule as .level. NULL for assignments created before migration 0095.';
COMMENT ON COLUMN ta_request_assignments.student_id_snapshot IS
    'Denormalized copy of ta_enrollments.student_id at assignment-creation time. Source of truth is still enrollment_id -> ta_enrollments.student_id; this column exists so read sites that already carry `a` in scope don''t need a new JOIN.';

-- Best-effort backfill for existing rows: only unambiguous when a user has
-- EXACTLY ONE enrollment period today (true for everyone right after 0094's
-- backfill, since there are no real transition cases yet) — there's no
-- created_at on ta_request_assignments to match against a point in time, so
-- a user with more than one period is left NULL rather than guessed at.
UPDATE ta_request_assignments a
SET enrollment_id = e.id, student_id_snapshot = e.student_id
FROM ta_enrollments e
WHERE e.user_id = a.ta_id
  AND a.enrollment_id IS NULL
  AND (SELECT COUNT(*) FROM ta_enrollments e2 WHERE e2.user_id = a.ta_id) = 1;
