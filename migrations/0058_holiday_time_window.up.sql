-- 0058 — Partial-day holidays (วันหยุดแบบระบุช่วงเวลา), for faculty closures.
--
-- Why: a คณะ holiday is usually not a whole day off. The college runs a ceremony
-- in the morning and teaching resumes after lunch. Recorded as a full-day row —
-- the only shape the table could hold — the system killed BOTH periods of that
-- day, forced the lecturer to file a makeup for a class that was never actually
-- cancelled, and then refused the most natural makeup slot of all: the free half
-- of that same day, because the whole date read as closed.
--
-- Model: start_time/end_time NULL = all-day. Every row that exists today is
-- NULL/NULL, so nothing about current behaviour changes. When the window is set,
-- the holiday blocks only class periods and work-log entries that OVERLAP
-- [start_time, end_time); anything outside it is a normal teaching slot and a
-- legal makeup target — including later the same day.
--
-- The overlap predicate lives in three places that must agree (they are the same
-- half-open comparison in all three):
--   * internal/service/worklog.go       — holidaySet.overlap (validator + generator)
--   * internal/service/holiday.go       — ImpactsForCourse
--   * internal/service/course_window.go — UnresolvedMakeupsSQL

ALTER TABLE public_holidays
    ADD COLUMN start_time TIME,
    ADD COLUMN end_time   TIME;

-- Both or neither, and non-empty. A half-set window would read as "all day" in
-- some queries and as a window in others.
ALTER TABLE public_holidays
    ADD CONSTRAINT public_holidays_window_check
    CHECK ((start_time IS NULL AND end_time IS NULL)
        OR (start_time IS NOT NULL AND end_time IS NOT NULL AND end_time > start_time));

-- UNIQUE (holiday_date, source) has to widen: one date can now legitimately carry
-- two faculty windows (a morning ceremony and a late-afternoon event are separate
-- closures with teaching in between). COALESCE keeps the all-day row unique per
-- source — bare NULLs compare as distinct under the default NULLS DISTINCT, which
-- would let the same national holiday be inserted over and over.
--
-- The original constraint was declared inline in 0023, so its name is
-- system-generated; find it rather than guessing (same approach as 0035).
DO $$
DECLARE
    con_name text;
BEGIN
    SELECT conname INTO con_name
    FROM pg_constraint
    WHERE conrelid = 'public_holidays'::regclass
      AND contype = 'u'
      AND pg_get_constraintdef(oid) ILIKE '%holiday_date%source%';
    IF con_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE public_holidays DROP CONSTRAINT %I', con_name);
    END IF;
END $$;

CREATE UNIQUE INDEX uq_public_holidays_date_source_window
    ON public_holidays (holiday_date, source, (COALESCE(start_time, TIME '00:00')));
