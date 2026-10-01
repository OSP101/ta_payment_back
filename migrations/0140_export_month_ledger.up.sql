-- What each exported (course, TA, month, track) was paid, frozen at export.
--
-- Exporting is the freeze point for a month's money, but until now nothing
-- was frozen except the graduate-special lump (grad_lump_ledger, 0119): hourly
-- pay was re-priced live on every read, from the rate in force and from a
-- budget settlement that shares a short pool across the WHOLE term. A later
-- month's approvals, a rate edit or a settlement-mode change therefore moved
-- the figure of a month finance already held (CP363205: exported มิ.ย.–ก.ย. at
-- 21,330.34, recomputed at 21,591), and the drift check then refused even a
-- plain re-download with 409.
--
-- A row here is what the claim document said for that month: earned_baht is
-- the cost before the budget cut, paid_baht what the settlement funded. Every
-- reader of a locked month prices it from here (claimCostByTASlot scales the
-- month's คาบ to earned_baht; the settlement takes paid_baht off the top of the
-- pool before sharing the rest), so the preview, the dashboards, the transfer
-- cover and the course summary all agree with the document.
--
-- Append-only, one row per export that locked the cell. The row in force is
-- the newest one written no earlier than the cell's own exported_at: a month
-- sent back and exported again (a corrected version) gets a new row, and the
-- old one stays as history of what the earlier document said. A month that is
-- not exported/finance_sent right now is priced live, whatever is stored.
--
-- year_month is GREGORIAN ("2026-08"), matching work_logs.work_date and
-- export_batches.months — not the Buddhist academic key of submission_periods.
--
-- Months exported before this table existed have no row and cannot be
-- backfilled exactly (the figure they printed was never stored per TA). Those
-- are re-served from the archived ZIP (export_batches.file_path) on a
-- re-download, and the preview shows the archived batch total.
CREATE TABLE export_month_ledger (
    id                 UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    teaching_course_id UUID          NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    ta_id              UUID          NOT NULL REFERENCES users(id),
    year_month         TEXT          NOT NULL CHECK (year_month ~ '^[0-9]{4}-[0-9]{2}$'),
    track              TEXT          NOT NULL CHECK (track IN ('regular','special')),
    earned_baht        NUMERIC(12,2) NOT NULL CHECK (earned_baht >= 0),
    paid_baht          NUMERIC(12,2) NOT NULL CHECK (paid_baht >= 0),
    frozen_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    frozen_by          UUID          REFERENCES users(id)
);
CREATE INDEX export_month_ledger_cell_idx
    ON export_month_ledger (teaching_course_id, ta_id, year_month, track, frozen_at DESC);

-- A deliberate send-back of an exported month ends in a corrected document.
-- It is a new version of the claim, not a reprint: version counts them per
-- course and month slice, previous_batch_id points at the document it
-- replaces, and the replaced batch stays in the history.
ALTER TABLE export_batches
    ADD COLUMN IF NOT EXISTS version INT NOT NULL DEFAULT 1 CHECK (version >= 1),
    ADD COLUMN IF NOT EXISTS previous_batch_id UUID REFERENCES export_batches(id) ON DELETE SET NULL;
