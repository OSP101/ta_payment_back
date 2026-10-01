-- A TA who added a class to their own timetable after a request was approved
-- could not take it back: assertNoClassRemovedAfterApproval refused removing
-- ANY class once an assignment was approved, because removal can win back
-- trimmed teaching hours. A class typed in by mistake after approval is not
-- that case — it was never part of any decision — but the table had no way to
-- tell the two apart, so the TA was stuck until staff stepped in.
--
-- added_after_approval marks a row first saved while the TA already held an
-- approved assignment for the term; created_at says when, so a row that a
-- LATER approval then took into account is protected again. Existing rows are
-- backfilled as protected (FALSE) — the safe reading for rows of unknown age.
-- The save path carries both values forward when a block is kept or edited,
-- since the timetable is replaced wholesale on every save.
ALTER TABLE ta_class_schedules
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS added_after_approval BOOLEAN NOT NULL DEFAULT FALSE;
