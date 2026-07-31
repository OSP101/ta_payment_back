-- Phase 1 of the 24/07/2026 meeting plan: the TA request stops being decided
-- the instant it is submitted.
--
-- Three changes travel together because they are one behaviour:
--
--   1. A request may now REST in 'submitted' while the system waits for every
--      TA on it to record their class timetable. There is no new status value:
--      'submitted' already exists in ta_request_status and already means
--      exactly this ("handed in, not yet judged"). The auto-decide model
--      introduced in 0017 simply never left a row there. Reusing it avoids an
--      ALTER TYPE ... ADD VALUE, whose new label cannot be used in the same
--      transaction as the migration that adds it.
--
--   2. The verdict is no longer one flag for the whole request. A TA whose own
--      class overlaps some of a section's sessions keeps the rest, so the
--      outcome is per (TA, section): active / trimmed / dropped.
--
--   3. The lecturer now declares hours PER SECTION rather than one figure for
--      the whole request, so every assignment needs its own workload row.

-- ---------------------------------------------------------------------------
-- 1. Per-assignment outcome
-- ---------------------------------------------------------------------------
CREATE TYPE ta_assignment_state AS ENUM ('active', 'trimmed', 'dropped');

ALTER TABLE ta_request_assignments
    ADD COLUMN IF NOT EXISTS state ta_assignment_state NOT NULL DEFAULT 'active',
    -- Why this section was trimmed or dropped, in Thai, ready to show the TA
    -- and the lecturer. NULL while state = 'active'.
    ADD COLUMN IF NOT EXISTS state_reason TEXT,
    ADD COLUMN IF NOT EXISTS state_decided_at TIMESTAMPTZ,
    -- Sections taught in the same slot to different groups. One TA covering
    -- them works those hours ONCE, so the group is billed once even though it
    -- appears as several assignment rows. NULL = not co-taught.
    ADD COLUMN IF NOT EXISTS cotaught_group INT;

COMMENT ON COLUMN ta_request_assignments.state IS
    'active = TA works every session of this section; trimmed = some sessions are unavailable (their own class wins); dropped = every session clashes, TA is out of this section and its quota is released';

-- The quota rule counts distinct courses per TA across live requests, and the
-- re-evaluation sweep looks up pending assignments by TA. Both hit this.
CREATE INDEX IF NOT EXISTS ta_request_assignments_ta_state_idx
    ON ta_request_assignments (ta_id, state);

-- ---------------------------------------------------------------------------
-- 2. Per-section workload
--
-- Before this migration Create() wrote ta_workload_forms only for a TA's FIRST
-- section in a request (`if j != 0 { continue }`), so the hours meant "all of
-- this TA's sections combined".
--
-- Backfill copies those hours to the sibling assignments and puts them in one
-- cotaught_group. Copying rather than dividing is deliberate: the declared
-- figure described work the TA does once while covering several groups at the
-- same time, and the group marker is what stops it being billed N times. A
-- naive split would silently cut every historical TA's declared workload.
-- ---------------------------------------------------------------------------
WITH declared AS (
    -- The one form row per (request, TA), plus the assignment it hangs off.
    SELECT a.request_id,
           a.ta_id,
           w.help_teach_hrs, w.help_teach_desc,
           w.prep_hrs, w.prep_desc,
           w.grade_hrs, w.grade_desc,
           w.other_hrs, w.other_desc,
           w.check_work_hrs, w.attendance_hrs,
           w.ug_other_hrs, w.ug_other_desc,
           w.lab_hrs
    FROM ta_request_assignments a
    JOIN ta_workload_forms w ON w.assignment_id = a.id
),
missing AS (
    -- Sibling assignments of the same (request, TA) that have no form yet.
    SELECT a.id AS assignment_id, d.*
    FROM ta_request_assignments a
    JOIN declared d ON d.request_id = a.request_id AND d.ta_id = a.ta_id
    WHERE NOT EXISTS (SELECT 1 FROM ta_workload_forms w WHERE w.assignment_id = a.id)
)
INSERT INTO ta_workload_forms (
    id, assignment_id, help_teach_hrs, help_teach_desc, prep_hrs, prep_desc,
    grade_hrs, grade_desc, other_hrs, other_desc,
    check_work_hrs, attendance_hrs, ug_other_hrs, ug_other_desc, lab_hrs)
SELECT gen_random_uuid(), assignment_id, help_teach_hrs, help_teach_desc,
       prep_hrs, prep_desc, grade_hrs, grade_desc, other_hrs, other_desc,
       check_work_hrs, attendance_hrs, ug_other_hrs, ug_other_desc, lab_hrs
FROM missing;

-- Mark every pre-existing multi-section TA as co-taught, preserving the
-- "worked once, billed once" meaning the single shared form used to carry.
-- Group number is per request, so 1 is enough for the historical case.
UPDATE ta_request_assignments a
SET cotaught_group = 1
WHERE EXISTS (
    SELECT 1 FROM ta_request_assignments b
    WHERE b.request_id = a.request_id
      AND b.ta_id = a.ta_id
      AND b.id <> a.id
);
