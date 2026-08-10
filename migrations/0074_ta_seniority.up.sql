-- 09/08/2026 — the "ปะหน้าจ่ายตรง" transfer-cover sheet marks each TA new or
-- returning. Staff's own definition: new = never held an account, never
-- submitted a document, in this system before. Storing THAT as a plain
-- boolean would have to be re-flipped for every TA at the start of every term,
-- and printing an old term's document again after the flip would show today's
-- answer instead of the answer that was true when the term ran.
--
-- Storing the first term instead answers both problems at once: the label for
-- any given document is `ta_first_term_id = that term ? 'ใหม่' : 'เก่า'`, which
-- stays correct no matter how many terms have passed since.
--
-- ta_first_term_id is written once (first time approved) and never overwritten
-- automatically after that — see the taSeniority() resolver. It is NULL for
-- everyone imported before this column existed, which is why the override
-- exists: those people's real first term predates this system, and nothing
-- here can recover that, so staff sets it by hand once per person.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS ta_first_term_id UUID REFERENCES academic_terms(id),
    ADD COLUMN IF NOT EXISTS ta_seniority_override TEXT
        CHECK (ta_seniority_override IN ('new','returning'));

COMMENT ON COLUMN users.ta_first_term_id IS
    'The academic_terms.id of this person''s first term as an appointed TA in this system. Set once, never auto-overwritten. NULL = predates this system or never appointed yet.';
COMMENT ON COLUMN users.ta_seniority_override IS
    'Staff-set ใหม่/เก่า label that wins over ta_first_term_id entirely — needed for anyone whose real TA history predates this system, where ta_first_term_id cannot be trusted.';
