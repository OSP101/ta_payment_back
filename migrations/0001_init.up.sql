-- COCO TA Payment schema (Phase 1 + 2 + 3)
-- All tables use UUID primary keys, timestamps in UTC, soft-delete via deleted_at.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "citext";

-- ============================================================================
-- Enum types
-- ============================================================================
CREATE TYPE role_code AS ENUM ('admin', 'staff', 'lecturer', 'ta');
CREATE TYPE study_level AS ENUM ('undergrad', 'master', 'phd');
CREATE TYPE section_track AS ENUM ('regular', 'special');
CREATE TYPE ta_request_status AS ENUM ('draft', 'submitted', 'approved', 'rejected', 'cancelled');
CREATE TYPE doc_status AS ENUM ('pending', 'submitted', 'approved', 'rejected', 'needs_fix');
CREATE TYPE worklog_status AS ENUM ('draft', 'submitted', 'approved', 'rejected');
CREATE TYPE reimburse_scope AS ENUM ('lecture', 'lab', 'both');
CREATE TYPE exam_kind AS ENUM ('mid_lecture', 'final_lecture', 'mid_lab', 'final_lab');
CREATE TYPE notification_channel AS ENUM ('in_app', 'email');

-- ============================================================================
-- 1. Users, roles, sessions, audit
-- ============================================================================
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         CITEXT NOT NULL UNIQUE,
    first_name    TEXT NOT NULL,
    last_name     TEXT NOT NULL,
    phone         TEXT,
    password_hash TEXT,
    sso_subject   TEXT UNIQUE,
    study_level   study_level,
    student_id    TEXT,
    department    TEXT,
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    profile_completed BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);
CREATE INDEX users_active_idx ON users (is_active) WHERE deleted_at IS NULL;

CREATE TABLE user_roles (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role    role_code NOT NULL,
    PRIMARY KEY (user_id, role)
);

CREATE TABLE audit_logs (
    id         BIGSERIAL PRIMARY KEY,
    at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    actor_id   UUID REFERENCES users(id),
    actor_role role_code,
    action     TEXT NOT NULL,
    entity     TEXT NOT NULL,
    entity_id  TEXT,
    ip         INET,
    user_agent TEXT,
    before     JSONB,
    after      JSONB,
    note       TEXT
);
CREATE INDEX audit_at_idx     ON audit_logs (at DESC);
CREATE INDEX audit_actor_idx  ON audit_logs (actor_id, at DESC);
CREATE INDEX audit_entity_idx ON audit_logs (entity, entity_id);

-- ============================================================================
-- 2. Academic reference data
-- ============================================================================
CREATE TABLE academic_terms (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    academic_year INT NOT NULL,       -- Buddhist year (e.g. 2568)
    semester      INT NOT NULL,       -- 1, 2, 3 (summer)
    starts_on     DATE,
    ends_on       DATE,
    is_active     BOOLEAN NOT NULL DEFAULT FALSE,
    UNIQUE (academic_year, semester)
);

CREATE TABLE faculty_courses (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code         TEXT NOT NULL UNIQUE,
    name_th      TEXT NOT NULL,
    name_en      TEXT,
    credits      INT NOT NULL,
    lecture_hrs  INT NOT NULL DEFAULT 0,
    lab_hrs      INT NOT NULL DEFAULT 0,
    self_hrs     INT NOT NULL DEFAULT 0,
    department   TEXT,
    is_active    BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Pay rate settings (versioned)
CREATE TABLE pay_rates (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    effective_from    DATE NOT NULL,
    undergrad_regular NUMERIC(10,2) NOT NULL,   -- baht/hour
    undergrad_special NUMERIC(10,2) NOT NULL,   -- baht/hour
    graduate_regular  NUMERIC(10,2) NOT NULL,   -- baht/hour
    graduate_special_lumpsum NUMERIC(10,2) NOT NULL, -- baht/person/month
    note              TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Hour caps by credits
CREATE TABLE hour_caps (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    credits     INT NOT NULL UNIQUE,
    hours_cap   INT NOT NULL,
    note        TEXT
);

-- Budget caps per course (baht)
CREATE TABLE budget_caps (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    effective_from DATE NOT NULL,
    per_course_max NUMERIC(10,2) NOT NULL,      -- e.g. 20000
    note           TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================================
-- 3. Teaching courses (course offerings), sections and schedules
-- ============================================================================
CREATE TABLE teaching_courses (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    faculty_course_id UUID NOT NULL REFERENCES faculty_courses(id),
    term_id           UUID NOT NULL REFERENCES academic_terms(id),
    starts_on         DATE,
    ends_on           DATE,
    num_students      INT NOT NULL DEFAULT 0,
    created_by        UUID REFERENCES users(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (faculty_course_id, term_id)
);

CREATE TABLE teaching_lecturers (
    teaching_course_id UUID NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    lecturer_id        UUID NOT NULL REFERENCES users(id),
    is_primary         BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (teaching_course_id, lecturer_id)
);

CREATE TABLE sections (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    teaching_course_id UUID NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    sec_no             TEXT NOT NULL,
    track              section_track NOT NULL DEFAULT 'regular',
    room               TEXT,
    UNIQUE (teaching_course_id, sec_no)
);

CREATE TABLE section_schedules (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    section_id   UUID NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('lecture', 'lab')),
    day_of_week  INT NOT NULL CHECK (day_of_week BETWEEN 0 AND 6),
    start_time   TIME NOT NULL,
    end_time     TIME NOT NULL,
    room         TEXT
);
CREATE INDEX section_schedules_section_idx ON section_schedules (section_id);

CREATE TABLE exam_schedules (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    section_id UUID NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
    kind       exam_kind NOT NULL,
    exam_date  DATE NOT NULL,
    start_time TIME,
    end_time   TIME,
    room       TEXT
);

CREATE TABLE makeup_schedules (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    section_id    UUID NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
    original_date DATE NOT NULL,
    makeup_date   DATE NOT NULL,
    start_time    TIME,
    end_time      TIME,
    note          TEXT
);

CREATE TABLE lecture_review_dates (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    section_id UUID NOT NULL REFERENCES sections(id) ON DELETE CASCADE,
    review_date DATE NOT NULL,
    start_time TIME,
    end_time   TIME,
    hours      NUMERIC(4,2) NOT NULL,
    note       TEXT
);

-- Excel schedule import history
CREATE TABLE schedule_imports (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    imported_by  UUID REFERENCES users(id),
    filename     TEXT NOT NULL,
    row_count    INT NOT NULL,
    error_count  INT NOT NULL,
    summary      JSONB,
    at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================================
-- 4. TA nomination workflow
-- ============================================================================
CREATE TABLE ta_request_windows (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id       UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    opens_at      TIMESTAMPTZ NOT NULL,
    closes_at     TIMESTAMPTZ NOT NULL,
    is_open       BOOLEAN NOT NULL DEFAULT TRUE,
    note          TEXT
);

CREATE TABLE ta_requests (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    teaching_course_id UUID NOT NULL REFERENCES teaching_courses(id),
    window_id          UUID REFERENCES ta_request_windows(id),
    lecturer_id        UUID NOT NULL REFERENCES users(id),
    reimburse_scope    reimburse_scope NOT NULL,
    status             ta_request_status NOT NULL DEFAULT 'draft',
    submitted_at       TIMESTAMPTZ,
    decided_at         TIMESTAMPTZ,
    decided_by         UUID REFERENCES users(id),
    reject_reason      TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-section requested count (undergrad+graduate)
CREATE TABLE ta_request_counts (
    request_id       UUID NOT NULL REFERENCES ta_requests(id) ON DELETE CASCADE,
    section_id       UUID NOT NULL REFERENCES sections(id),
    undergrad_count  INT NOT NULL DEFAULT 0,
    graduate_count   INT NOT NULL DEFAULT 0,
    PRIMARY KEY (request_id, section_id)
);

-- Assigned TA per section
CREATE TABLE ta_request_assignments (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id UUID NOT NULL REFERENCES ta_requests(id) ON DELETE CASCADE,
    section_id UUID NOT NULL REFERENCES sections(id),
    ta_id      UUID NOT NULL REFERENCES users(id),
    level      study_level NOT NULL,
    UNIQUE (request_id, section_id, ta_id)
);

-- Workload structure (per request, per TA)
CREATE TABLE ta_workload_forms (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    assignment_id UUID NOT NULL REFERENCES ta_request_assignments(id) ON DELETE CASCADE,
    -- graduate style tasks
    help_teach_hrs      NUMERIC(4,2) DEFAULT 0,
    help_teach_desc     TEXT,
    prep_hrs            NUMERIC(4,2) DEFAULT 0,
    prep_desc           TEXT,
    grade_hrs           NUMERIC(4,2) DEFAULT 0,
    grade_desc          TEXT,
    other_hrs           NUMERIC(4,2) DEFAULT 0,
    other_desc          TEXT,
    -- undergrad lecture-slot tasks
    check_work_hrs      NUMERIC(4,2) DEFAULT 0,
    attendance_hrs      NUMERIC(4,2) DEFAULT 0,
    ug_other_hrs        NUMERIC(4,2) DEFAULT 0,
    ug_other_desc       TEXT,
    -- undergrad lab-slot
    lab_hrs             NUMERIC(4,2) DEFAULT 0,
    UNIQUE (assignment_id)
);

-- ============================================================================
-- 5. TA profile & documents
-- ============================================================================
CREATE TABLE ta_profiles (
    user_id       UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    national_id   TEXT,
    bank_name     TEXT,
    bank_branch   TEXT,
    branch_code   TEXT,
    account_no    TEXT,
    account_name  TEXT,
    signature_svg TEXT,
    completed_at  TIMESTAMPTZ,
    verified_at   TIMESTAMPTZ,
    verified_by   UUID REFERENCES users(id),
    reject_reason TEXT,
    status        doc_status NOT NULL DEFAULT 'pending'
);

CREATE TABLE ta_documents (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL, -- 'national_id', 'bank_book', 'creditor_form'
    filename   TEXT NOT NULL,
    mime       TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    storage_key TEXT NOT NULL,
    status     doc_status NOT NULL DEFAULT 'submitted',
    reject_reason TEXT,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reviewed_at TIMESTAMPTZ,
    reviewed_by UUID REFERENCES users(id)
);
CREATE INDEX ta_documents_user_idx ON ta_documents (user_id, kind);

-- TA class schedule (used to check conflict)
CREATE TABLE ta_class_schedules (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    term_id    UUID NOT NULL REFERENCES academic_terms(id),
    course_label TEXT,
    day_of_week INT NOT NULL CHECK (day_of_week BETWEEN 0 AND 6),
    start_time TIME NOT NULL,
    end_time   TIME NOT NULL,
    note       TEXT,
    is_wba     BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX ta_class_user_term_idx ON ta_class_schedules (user_id, term_id);

-- ============================================================================
-- 6. Work logs (บันทึกเวลาปฏิบัติงาน)
-- ============================================================================
CREATE TABLE work_logs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    assignment_id UUID NOT NULL REFERENCES ta_request_assignments(id) ON DELETE CASCADE,
    work_date     DATE NOT NULL,
    start_time    TIME NOT NULL,
    end_time      TIME NOT NULL,
    hours         NUMERIC(4,2) NOT NULL,
    activity      TEXT NOT NULL,  -- 'lecture' | 'lab' | 'review' | 'makeup' | 'other'
    room          TEXT,
    note          TEXT,
    status        worklog_status NOT NULL DEFAULT 'draft',
    submitted_at  TIMESTAMPTZ,
    approved_at   TIMESTAMPTZ,
    approved_by   UUID REFERENCES users(id),
    reject_reason TEXT
);
CREATE INDEX work_logs_assign_idx ON work_logs (assignment_id, work_date);

-- ============================================================================
-- 7. Notifications
-- ============================================================================
CREATE TABLE notifications (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel    notification_channel NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    link       TEXT,
    read_at    TIMESTAMPTZ,
    sent_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX notifications_user_idx ON notifications (user_id, created_at DESC);

-- ============================================================================
-- 8. Announcements
-- ============================================================================
CREATE TABLE announcements (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title       TEXT NOT NULL,
    body        TEXT NOT NULL,
    audience    role_code[],
    published_at TIMESTAMPTZ,
    created_by  UUID REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================================
-- 9. Exports
-- ============================================================================
CREATE TABLE exports (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id      UUID REFERENCES academic_terms(id),
    scope        TEXT NOT NULL, -- 'course' | 'term'
    scope_id     TEXT,
    filename     TEXT NOT NULL,
    storage_key  TEXT NOT NULL,
    size_bytes   BIGINT NOT NULL,
    created_by   UUID REFERENCES users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================================
-- Seed roles for a fresh install (empty by default; admin created via CLI/env)
-- ============================================================================
