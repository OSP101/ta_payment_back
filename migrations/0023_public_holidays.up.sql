-- 0023 — Public holidays + makeup-reminder log + tighten makeup uniqueness.
--
-- Why: the TA-worklog validator (see internal/service/worklog.go) has no way
-- to know that a scheduled class day fell on a national holiday. This was the
-- root cause of the paper-workflow complaints: TAs logged and got paid for
-- hours the university was closed. See plans/1-sharded-hopcroft.md.
--
-- Notes:
--   * We store concrete dates only. Thai lunar/cabinet-announced holidays
--     shift every year; a recurrence rule would silently produce wrong days.
--   * `source` lets staff distinguish untouchable ("national") entries from
--     university-added closures ("university") and one-off overrides
--     ("custom") in the admin UI.
--   * `UNIQUE (holiday_date, source)` allows the same date to appear once
--     per source (e.g. a national holiday + a university-specific closure on
--     the same day).
--   * `holiday_remind_log` powers the 24h rate limit on the TA→lecturer
--     "please file a makeup" nag button. Primary key is compound so
--     ON CONFLICT logic is trivial in the service.

CREATE TABLE public_holidays (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    holiday_date DATE NOT NULL,
    name_th      TEXT NOT NULL,
    name_en      TEXT,
    source       TEXT NOT NULL DEFAULT 'national'
                 CHECK (source IN ('national', 'university', 'custom')),
    note         TEXT,
    created_by   UUID REFERENCES users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (holiday_date, source)
);
CREATE INDEX ix_public_holidays_date ON public_holidays(holiday_date);

CREATE TABLE holiday_remind_log (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ta_id              UUID NOT NULL REFERENCES users(id),
    teaching_course_id UUID NOT NULL REFERENCES teaching_courses(id),
    original_date      DATE NOT NULL,
    note               TEXT,
    sent_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX ix_hrl_recent
    ON holiday_remind_log (ta_id, teaching_course_id, original_date, sent_at DESC);

-- Prevent two makeup rows for the same (section, original_date). Editing a
-- makeup means DELETE + INSERT (both audited) — simpler than a partial UPDATE
-- that would need to re-validate everything.
ALTER TABLE makeup_schedules
    ADD CONSTRAINT uq_makeup_section_original UNIQUE (section_id, original_date);
