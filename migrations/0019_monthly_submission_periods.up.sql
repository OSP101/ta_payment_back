-- 0019_monthly_submission_periods.up.sql
-- Phase 3: monthly submission workflow. Staff creates 5 periods (มิ.ย.–ต.ค.)
-- per term; TAs sign each month; scheduler yields reminders 3 days before due;
-- exports scope to a submission period.

CREATE TABLE IF NOT EXISTS submission_periods (
    id                 UUID PRIMARY KEY,
    term_id            UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    year_month         CHAR(7) NOT NULL,          -- '2569-06'
    due_date           DATE    NOT NULL,          -- '2569-07-31'
    label              TEXT    NOT NULL,          -- 'มิถุนายน 2569'
    remind_days_before INT     NOT NULL DEFAULT 3,
    is_closed          BOOLEAN NOT NULL DEFAULT FALSE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (term_id, year_month)
);

CREATE INDEX IF NOT EXISTS submission_periods_due_idx
    ON submission_periods (due_date, is_closed);

-- Per (period × TA × teaching_course) status row so a TA can see, and the
-- scheduler can page on, exactly which cells are still pending. Rows are
-- lazily inserted the first time a TA opens the reminders page or when the
-- scheduler enumerates pending items.
CREATE TABLE IF NOT EXISTS submission_period_status (
    id                   UUID PRIMARY KEY,
    submission_period_id UUID NOT NULL REFERENCES submission_periods(id) ON DELETE CASCADE,
    ta_id                UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    teaching_course_id   UUID NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    status               TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','ta_signed','lecturer_signed','submitted','skipped')),
    ta_signed_at         TIMESTAMPTZ,
    lecturer_signed_at   TIMESTAMPTZ,
    submitted_at         TIMESTAMPTZ,
    last_reminded_at     TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (submission_period_id, ta_id, teaching_course_id)
);

CREATE INDEX IF NOT EXISTS submission_period_status_ta_idx
    ON submission_period_status (ta_id, status);
