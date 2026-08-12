-- 12/08/2026 — a term that crosses the 30 ก.ย. budget-year boundary now issues
-- TWO claim documents (see migration 0079/0080's fiscal-year split), and each
-- one is a separate physical folder that TA → อาจารย์ → ผู้รับรอง sign and staff
-- route to finance independently. Until now document_progress and
-- signature_checklist tracked ONE journey per term, so signing round 2's
-- document would either collide with round 1's ticks or be impossible to
-- record at all.
--
-- fiscal_round is 1 for the closing budget year's document (or the whole term,
-- for a term that does not cross the boundary — the common case, unaffected)
-- and 2 for the new budget year's document (ตุลาคม only, so far). It is a
-- small integer rather than a reference to any month/date row because the
-- round is derived on the fly from TermMonths + fiscalSplit — there is no
-- "fiscal_rounds" table to point at, and there does not need to be one.
--
-- DEFAULT 1 on both tables means every existing row becomes round 1
-- automatically: a term that never crossed a budget year always had exactly
-- one journey, and that journey IS round 1 now.

ALTER TABLE document_progress DROP CONSTRAINT document_progress_pkey;
ALTER TABLE document_progress ADD COLUMN fiscal_round INT NOT NULL DEFAULT 1 CHECK (fiscal_round IN (1, 2));
ALTER TABLE document_progress ADD PRIMARY KEY (term_id, fiscal_round);

ALTER TABLE signature_checklist ADD COLUMN fiscal_round INT NOT NULL DEFAULT 1 CHECK (fiscal_round IN (1, 2));

-- Same reasoning as 0065: two partial uniques instead of one, because NULL
-- signer_id (the certifier) does not collide with itself in a plain unique
-- index — fiscal_round just joins the existing key on both.
DROP INDEX IF EXISTS ux_signature_checklist_person;
DROP INDEX IF EXISTS ux_signature_checklist_course_role;
CREATE UNIQUE INDEX ux_signature_checklist_person
    ON signature_checklist (teaching_course_id, role, signer_id, fiscal_round)
    WHERE signer_id IS NOT NULL;
CREATE UNIQUE INDEX ux_signature_checklist_course_role
    ON signature_checklist (teaching_course_id, role, fiscal_round)
    WHERE signer_id IS NULL;

COMMENT ON COLUMN document_progress.fiscal_round IS
    '1 = the whole term, or the closing budget year''s document for a term that crosses 30 ก.ย.; 2 = the new budget year''s document (currently ตุลาคม only).';
COMMENT ON COLUMN signature_checklist.fiscal_round IS
    'Which document journey this signature belongs to — see document_progress.fiscal_round.';
