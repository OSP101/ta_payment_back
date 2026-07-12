-- TA request auto-decision checklist.
--
-- Motivation:
--   Officer manual approval is being replaced by automatic system decisions.
--   The rule set (docs+schedule eligibility, 3-course cap, section time
--   conflicts, workload validity) already lives in TARequestService.Approve;
--   we now run it inside the same tx as Create and store the per-rule outcome
--   so the staff-side accordion can display the checklist that produced the
--   verdict without re-running the rules on read.
--
-- Semantics:
--   - decision_checks: JSON array of {rule, ta, passed, message}. Non-empty
--     for auto-decided rows (decided_by IS NULL); legacy human decisions keep
--     the default '[]'.
--   - decided_by IS NULL now indicates a system decision. Existing rows with
--     non-null decided_by are legacy manual decisions.

ALTER TABLE ta_requests
    ADD COLUMN IF NOT EXISTS decision_checks JSONB NOT NULL DEFAULT '[]'::jsonb;
