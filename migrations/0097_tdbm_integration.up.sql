-- TDBM (tdbm.computing.kku.ac.th) integration — see docs/TDBM-API-requirements.md.
-- TDBM publishes two read endpoints (holidays, extra-teachings) plus a webhook
-- that pings us when either changes; we pull the current data on that ping and
-- on an hourly safety-net sweep (see internal/service/tdbm.go, scheduler.go).
--
-- WHY EXTRA-TEACHINGS LANDS IN A STAGING TABLE, NOT makeup_schedules DIRECTLY:
-- TDBM's /extra-teachings rows carry only raw internal ids — teacher_id,
-- class_id, teaching_id — with no subject code and no section number. There is
-- no reliable way from those alone to resolve which of our own `sections` row a
-- submission belongs to (TDBM has no /teachings or /classes endpoint to resolve
-- class_id against either). We've asked TDBM to add a subject code to this feed;
-- until they do, this mirrors what TDBM has, unmapped, for staff to read
-- (teacher name resolved via tdbm_teachers) rather than pretending to auto-file
-- it into makeup_schedules against a guess.

-- 1. Holidays CAN be folded straight into public_holidays: a holiday is not
-- tied to any one subject, and the existing (date, source, window) shape
-- already fits. tdbm_holiday_id is the sync key — matching by date+title text
-- would silently duplicate a row TDBM only renamed.
ALTER TABLE public_holidays
    ADD COLUMN tdbm_holiday_id INT UNIQUE;

DO $$
DECLARE
    con_name text;
BEGIN
    SELECT conname INTO con_name
    FROM pg_constraint
    WHERE conrelid = 'public_holidays'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) ILIKE '%source%';
    IF con_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE public_holidays DROP CONSTRAINT %I', con_name);
    END IF;
END $$;

ALTER TABLE public_holidays
    ADD CONSTRAINT public_holidays_source_check
    CHECK (source IN ('national', 'university', 'faculty', 'custom', 'tdbm'));

-- 2. Teachers mirror — small (~60 rows), refreshed on every sync. Only use so
-- far: resolve tdbm_extra_teachings.teacher_id to a name for staff to read.
CREATE TABLE tdbm_teachers (
    teacher_id      INT PRIMARY KEY,
    prefix          TEXT,
    "position"      TEXT,
    degree          TEXT,
    name            TEXT NOT NULL,
    email           TEXT,
    account_user_id INT,
    tdbm_created_at TIMESTAMPTZ,
    tdbm_updated_at TIMESTAMPTZ,
    synced_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 3. Extra-teachings (makeup submissions) staging mirror — one row per TDBM
-- extra_class_id, term-scoped since that's how we fetch (?academic_year=&
-- semester=). No FK to teaching_courses/sections: see the file header comment.
CREATE TABLE tdbm_extra_teachings (
    extra_class_id   INT PRIMARY KEY,
    academic_year    INT NOT NULL,
    semester         INT NOT NULL,
    title            TEXT,
    detail           TEXT,
    opt_status       TEXT,
    status           TEXT,
    class_date       DATE,
    start_time       TIME,
    end_time         TIME,
    duration_minutes INT,
    teacher_id       INT,
    holiday_id       INT,
    teaching_id      INT,
    class_id         INT,
    dbm_id           INT,
    etdoc_id         INT,
    created_user_id  INT,
    tdbm_created_at  TIMESTAMPTZ,
    tdbm_updated_at  TIMESTAMPTZ,
    synced_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_tdbm_extra_teachings_term ON tdbm_extra_teachings (academic_year, semester);
CREATE INDEX idx_tdbm_extra_teachings_teacher ON tdbm_extra_teachings (teacher_id);

-- 4. Sync log — one row per (resource, run), so a bad pull is visible after the
-- fact instead of only in a process log nobody was tailing at the time.
CREATE TABLE tdbm_sync_log (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource      TEXT NOT NULL CHECK (resource IN ('holidays', 'extra-teachings', 'teachers')),
    trigger_kind  TEXT NOT NULL CHECK (trigger_kind IN ('webhook', 'scheduler', 'manual')),
    academic_year INT,
    semester      INT,
    fetched       INT NOT NULL DEFAULT 0,
    inserted      INT NOT NULL DEFAULT 0,
    updated       INT NOT NULL DEFAULT 0,
    skipped       INT NOT NULL DEFAULT 0,
    error         TEXT,
    started_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_tdbm_sync_log_created ON tdbm_sync_log (created_at DESC);
