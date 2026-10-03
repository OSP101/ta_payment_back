-- เพดานงบรายวิชา (TOR §3.4: "ไม่เกิน 20,000 บาท หรือตามที่วิทยาลัยกำหนด").
--
-- ONE figure shared by every course of the term, set by staff in the term
-- settings (decision 02/10/2026) — not a per-course number. NULL = ไม่กำหนด:
-- every budget is the workload formula alone, exactly as before. A value is an
-- upper bound on top of the formula: BudgetService.Compute takes
-- min(formula, cap) and scales the regular/special split by the same ratio, so
-- the cap can only lower a course's budget, never raise it.
--
-- Per term rather than system-wide so changing the rule never re-prices a
-- term already paid out; a new term starts from the latest term's figure
-- (UpsertTerm), so staff set it once and only touch it when the rule changes.
ALTER TABLE academic_terms
    ADD COLUMN course_budget_cap_baht NUMERIC(12,2)
        CHECK (course_budget_cap_baht IS NULL OR course_budget_cap_baht >= 0);
