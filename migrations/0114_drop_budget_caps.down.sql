-- Restores the table shape only — not the code that used to write to it
-- (course.go's BudgetCap/LatestBudgetCap/UpsertBudgetCap and the
-- /settings/budget-cap routes), which this migration's .up counterpart was
-- removed alongside. A rollback gets the table back empty, not the feature.
CREATE TABLE IF NOT EXISTS budget_caps (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    effective_from DATE NOT NULL,
    per_course_max NUMERIC(10,2) NOT NULL,
    note           TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
