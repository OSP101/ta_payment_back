--
-- PostgreSQL database dump
--

\restrict t83TBLqMpNhuwQR6km9U9WSkjyTw8GN9ckBt3tiW0Kim5TYePoYaLpaoGhDJxVf

-- Dumped from database version 16.13
-- Dumped by pg_dump version 16.13

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: citext; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public;


--
-- Name: EXTENSION citext; Type: COMMENT; Schema: -; Owner: 
--

COMMENT ON EXTENSION citext IS 'data type for case-insensitive character strings';


--
-- Name: pgcrypto; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;


--
-- Name: EXTENSION pgcrypto; Type: COMMENT; Schema: -; Owner: 
--

COMMENT ON EXTENSION pgcrypto IS 'cryptographic functions';


--
-- Name: doc_status; Type: TYPE; Schema: public; Owner: itii_database_prod
--

CREATE TYPE public.doc_status AS ENUM (
    'pending',
    'submitted',
    'approved',
    'rejected',
    'needs_fix'
);


ALTER TYPE public.doc_status OWNER TO itii_database_prod;

--
-- Name: exam_kind; Type: TYPE; Schema: public; Owner: itii_database_prod
--

CREATE TYPE public.exam_kind AS ENUM (
    'mid_lecture',
    'final_lecture',
    'mid_lab',
    'final_lab'
);


ALTER TYPE public.exam_kind OWNER TO itii_database_prod;

--
-- Name: notification_channel; Type: TYPE; Schema: public; Owner: itii_database_prod
--

CREATE TYPE public.notification_channel AS ENUM (
    'in_app',
    'email'
);


ALTER TYPE public.notification_channel OWNER TO itii_database_prod;

--
-- Name: reimburse_scope; Type: TYPE; Schema: public; Owner: itii_database_prod
--

CREATE TYPE public.reimburse_scope AS ENUM (
    'lecture',
    'lab',
    'both'
);


ALTER TYPE public.reimburse_scope OWNER TO itii_database_prod;

--
-- Name: role_code; Type: TYPE; Schema: public; Owner: itii_database_prod
--

CREATE TYPE public.role_code AS ENUM (
    'admin',
    'staff',
    'lecturer',
    'ta'
);


ALTER TYPE public.role_code OWNER TO itii_database_prod;

--
-- Name: section_track; Type: TYPE; Schema: public; Owner: itii_database_prod
--

CREATE TYPE public.section_track AS ENUM (
    'regular',
    'special'
);


ALTER TYPE public.section_track OWNER TO itii_database_prod;

--
-- Name: study_level; Type: TYPE; Schema: public; Owner: itii_database_prod
--

CREATE TYPE public.study_level AS ENUM (
    'undergrad',
    'master',
    'phd'
);


ALTER TYPE public.study_level OWNER TO itii_database_prod;

--
-- Name: ta_request_status; Type: TYPE; Schema: public; Owner: itii_database_prod
--

CREATE TYPE public.ta_request_status AS ENUM (
    'draft',
    'submitted',
    'approved',
    'rejected',
    'cancelled'
);


ALTER TYPE public.ta_request_status OWNER TO itii_database_prod;

--
-- Name: worklog_status; Type: TYPE; Schema: public; Owner: itii_database_prod
--

CREATE TYPE public.worklog_status AS ENUM (
    'draft',
    'submitted',
    'approved',
    'rejected'
);


ALTER TYPE public.worklog_status OWNER TO itii_database_prod;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: academic_terms; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.academic_terms (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    academic_year integer NOT NULL,
    semester integer NOT NULL,
    starts_on date,
    ends_on date,
    is_active boolean DEFAULT false NOT NULL,
    months integer DEFAULT 4 NOT NULL,
    midterm_starts_on date,
    midterm_ends_on date,
    final_starts_on date,
    final_ends_on date
);


ALTER TABLE public.academic_terms OWNER TO itii_database_prod;

--
-- Name: admin_officers; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.admin_officers (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    academic_prefix text DEFAULT ''::text NOT NULL,
    full_name text NOT NULL,
    title text NOT NULL,
    is_active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE public.admin_officers OWNER TO itii_database_prod;

--
-- Name: announcements; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.announcements (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    title text NOT NULL,
    body text NOT NULL,
    audience public.role_code[],
    published_at timestamp with time zone,
    created_by uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    category text DEFAULT 'info'::text NOT NULL,
    pinned boolean DEFAULT false NOT NULL,
    cover_image_key text,
    expires_at timestamp with time zone,
    announced_at timestamp with time zone,
    updated_by uuid,
    CONSTRAINT announcements_category_check CHECK ((category = ANY (ARRAY['info'::text, 'news'::text, 'warning'::text, 'urgent'::text, 'event'::text])))
);


ALTER TABLE public.announcements OWNER TO itii_database_prod;

--
-- Name: audit_logs; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.audit_logs (
    id bigint NOT NULL,
    at timestamp with time zone DEFAULT now() NOT NULL,
    actor_id uuid,
    actor_role public.role_code,
    action text NOT NULL,
    entity text NOT NULL,
    entity_id text,
    ip inet,
    user_agent text,
    before jsonb,
    after jsonb,
    note text
);


ALTER TABLE public.audit_logs OWNER TO itii_database_prod;

--
-- Name: audit_logs_id_seq; Type: SEQUENCE; Schema: public; Owner: itii_database_prod
--

CREATE SEQUENCE public.audit_logs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


ALTER SEQUENCE public.audit_logs_id_seq OWNER TO itii_database_prod;

--
-- Name: audit_logs_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: itii_database_prod
--

ALTER SEQUENCE public.audit_logs_id_seq OWNED BY public.audit_logs.id;


--
-- Name: budget_caps; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.budget_caps (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    effective_from date NOT NULL,
    per_course_max numeric(10,2) NOT NULL,
    note text,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE public.budget_caps OWNER TO itii_database_prod;

--
-- Name: document_progress; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.document_progress (
    term_id uuid NOT NULL,
    stage integer DEFAULT 0 NOT NULL,
    ta_signed_at timestamp with time zone,
    lecturer_signed_at timestamp with time zone,
    certifier_signed_at timestamp with time zone,
    sent_finance_at timestamp with time zone,
    sent_treasury_at timestamp with time zone,
    note text,
    updated_by uuid,
    updated_by_name text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT document_progress_stage_check CHECK (((stage >= 0) AND (stage <= 5)))
);


ALTER TABLE public.document_progress OWNER TO itii_database_prod;

--
-- Name: exam_schedules; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.exam_schedules (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    section_id uuid NOT NULL,
    kind public.exam_kind NOT NULL,
    exam_date date NOT NULL,
    start_time time without time zone,
    end_time time without time zone,
    room text
);


ALTER TABLE public.exam_schedules OWNER TO itii_database_prod;

--
-- Name: export_batches; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.export_batches (
    id uuid NOT NULL,
    teaching_course_id uuid NOT NULL,
    submission_period_id uuid,
    file_path text NOT NULL,
    file_name text NOT NULL,
    ta_count integer DEFAULT 0 NOT NULL,
    total_baht numeric(12,2) DEFAULT 0 NOT NULL,
    generated_at timestamp with time zone DEFAULT now() NOT NULL,
    generated_by uuid NOT NULL
);


ALTER TABLE public.export_batches OWNER TO itii_database_prod;

--
-- Name: exports; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.exports (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    term_id uuid,
    scope text NOT NULL,
    scope_id text,
    filename text NOT NULL,
    storage_key text NOT NULL,
    size_bytes bigint NOT NULL,
    created_by uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE public.exports OWNER TO itii_database_prod;

--
-- Name: holiday_remind_log; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.holiday_remind_log (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    ta_id uuid NOT NULL,
    teaching_course_id uuid NOT NULL,
    original_date date NOT NULL,
    note text,
    sent_at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE public.holiday_remind_log OWNER TO itii_database_prod;

--
-- Name: lecture_review_dates; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.lecture_review_dates (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    section_id uuid NOT NULL,
    review_date date NOT NULL,
    start_time time without time zone,
    end_time time without time zone,
    hours numeric(4,2) NOT NULL,
    note text
);


ALTER TABLE public.lecture_review_dates OWNER TO itii_database_prod;

--
-- Name: makeup_schedules; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.makeup_schedules (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    section_id uuid NOT NULL,
    original_date date NOT NULL,
    makeup_date date NOT NULL,
    start_time time without time zone,
    end_time time without time zone,
    note text
);


ALTER TABLE public.makeup_schedules OWNER TO itii_database_prod;

--
-- Name: notifications; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.notifications (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid NOT NULL,
    channel public.notification_channel NOT NULL,
    title text NOT NULL,
    body text NOT NULL,
    link text,
    read_at timestamp with time zone,
    sent_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE public.notifications OWNER TO itii_database_prod;

--
-- Name: pay_rates; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.pay_rates (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    effective_from date NOT NULL,
    undergrad_regular numeric(10,2) NOT NULL,
    undergrad_special numeric(10,2) NOT NULL,
    graduate_regular numeric(10,2) NOT NULL,
    graduate_special_lumpsum numeric(10,2) NOT NULL,
    note text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    ug_lecture_hours_per_credit numeric(4,2) DEFAULT 3 NOT NULL,
    ug_lab_hours_per_credit numeric(4,2) DEFAULT 4.5 NOT NULL,
    baseline_students_lecture integer DEFAULT 60 NOT NULL,
    baseline_students_lab integer DEFAULT 30 NOT NULL,
    ug_workload_rate_regular numeric(10,2) DEFAULT 300 NOT NULL,
    ug_workload_rate_special numeric(10,2) DEFAULT 250 NOT NULL,
    term_months integer DEFAULT 4 NOT NULL,
    ug_max_hours_per_day integer DEFAULT 7 NOT NULL,
    max_courses_per_student integer DEFAULT 3 NOT NULL,
    graduate_regular_hourly numeric(10,2) DEFAULT 50 NOT NULL,
    grad_special_term_cap numeric(10,2) DEFAULT 12000 NOT NULL,
    daily_pay_cap_baht numeric(10,2) DEFAULT 300 NOT NULL,
    ug_regular_daily_hour_cap numeric(4,2) DEFAULT 7 NOT NULL,
    ug_special_daily_hour_cap numeric(4,2) DEFAULT 6 NOT NULL,
    grad_regular_daily_hour_cap numeric(4,2) DEFAULT 6 NOT NULL,
    ug_special_monthly_cap numeric(10,2) DEFAULT 2000 NOT NULL
);


ALTER TABLE public.pay_rates OWNER TO itii_database_prod;

--
-- Name: public_holidays; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.public_holidays (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    holiday_date date NOT NULL,
    name_th text NOT NULL,
    name_en text,
    source text DEFAULT 'national'::text NOT NULL,
    note text,
    created_by uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT public_holidays_source_check CHECK ((source = ANY (ARRAY['national'::text, 'university'::text, 'faculty'::text, 'custom'::text])))
);


ALTER TABLE public.public_holidays OWNER TO itii_database_prod;

--
-- Name: schedule_imports; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.schedule_imports (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    imported_by uuid,
    filename text NOT NULL,
    row_count integer NOT NULL,
    error_count integer NOT NULL,
    summary jsonb,
    at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE public.schedule_imports OWNER TO itii_database_prod;

--
-- Name: schema_migrations; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.schema_migrations (
    version text NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);


ALTER TABLE public.schema_migrations OWNER TO itii_database_prod;

--
-- Name: section_schedules; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.section_schedules (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    section_id uuid NOT NULL,
    kind text NOT NULL,
    day_of_week integer NOT NULL,
    start_time time without time zone NOT NULL,
    end_time time without time zone NOT NULL,
    room text,
    CONSTRAINT section_schedules_day_of_week_check CHECK (((day_of_week >= 0) AND (day_of_week <= 6))),
    CONSTRAINT section_schedules_kind_check CHECK ((kind = ANY (ARRAY['lecture'::text, 'lab'::text])))
);


ALTER TABLE public.section_schedules OWNER TO itii_database_prod;

--
-- Name: sections; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.sections (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    teaching_course_id uuid NOT NULL,
    sec_no text NOT NULL,
    track public.section_track DEFAULT 'regular'::public.section_track NOT NULL,
    room text,
    num_students integer DEFAULT 0 NOT NULL
);


ALTER TABLE public.sections OWNER TO itii_database_prod;

--
-- Name: signature_checklist; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.signature_checklist (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    term_id uuid NOT NULL,
    teaching_course_id uuid NOT NULL,
    role text NOT NULL,
    signed_at timestamp with time zone,
    updated_by uuid,
    updated_by_name text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT signature_checklist_role_check CHECK ((role = ANY (ARRAY['ta'::text, 'lecturer'::text, 'certifier'::text])))
);


ALTER TABLE public.signature_checklist OWNER TO itii_database_prod;

--
-- Name: submission_period_status; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.submission_period_status (
    id uuid NOT NULL,
    submission_period_id uuid NOT NULL,
    ta_id uuid NOT NULL,
    teaching_course_id uuid NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    ta_signed_at timestamp with time zone,
    lecturer_signed_at timestamp with time zone,
    submitted_at timestamp with time zone,
    last_reminded_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    ta_signed_name text,
    lecturer_signed_by uuid,
    lecturer_signed_name text,
    lecturer_comment text,
    staff_reviewed_by uuid,
    staff_reviewed_name text,
    staff_comment text,
    finance_sent_at timestamp with time zone,
    finance_sent_by uuid,
    finance_sent_name text,
    finance_note text,
    sent_back_at timestamp with time zone,
    sent_back_by uuid,
    sent_back_name text,
    sent_back_reason text,
    exported_at timestamp with time zone,
    exported_by uuid,
    exported_name text,
    CONSTRAINT submission_period_status_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'exported'::text, 'finance_sent'::text, 'skipped'::text])))
);


ALTER TABLE public.submission_period_status OWNER TO itii_database_prod;

--
-- Name: submission_periods; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.submission_periods (
    id uuid NOT NULL,
    term_id uuid NOT NULL,
    year_month character(7) NOT NULL,
    due_date date NOT NULL,
    label text NOT NULL,
    remind_days_before integer DEFAULT 3 NOT NULL,
    is_closed boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    starts_on date NOT NULL
);


ALTER TABLE public.submission_periods OWNER TO itii_database_prod;

--
-- Name: ta_class_schedules; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.ta_class_schedules (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid NOT NULL,
    term_id uuid NOT NULL,
    course_label text,
    day_of_week integer NOT NULL,
    start_time time without time zone NOT NULL,
    end_time time without time zone NOT NULL,
    note text,
    is_wba boolean DEFAULT false NOT NULL,
    course_code text,
    course_name text,
    kind text,
    sec_no text,
    CONSTRAINT ta_class_schedules_day_of_week_check CHECK (((day_of_week >= 0) AND (day_of_week <= 6))),
    CONSTRAINT ta_class_schedules_kind_check CHECK (((kind IS NULL) OR (kind = ANY (ARRAY['lecture'::text, 'lab'::text]))))
);


ALTER TABLE public.ta_class_schedules OWNER TO itii_database_prod;

--
-- Name: ta_documents; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.ta_documents (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid NOT NULL,
    kind text NOT NULL,
    filename text NOT NULL,
    mime text NOT NULL,
    size_bytes bigint NOT NULL,
    storage_key text NOT NULL,
    status public.doc_status DEFAULT 'submitted'::public.doc_status NOT NULL,
    reject_reason text,
    uploaded_at timestamp with time zone DEFAULT now() NOT NULL,
    reviewed_at timestamp with time zone,
    reviewed_by uuid,
    superseded_at timestamp with time zone,
    superseded_by uuid,
    round integer DEFAULT 1 NOT NULL,
    expires_at timestamp with time zone,
    file_deleted_at timestamp with time zone,
    reject_batch_id uuid
);


ALTER TABLE public.ta_documents OWNER TO itii_database_prod;

--
-- Name: ta_profile_submissions; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.ta_profile_submissions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id uuid NOT NULL,
    round integer NOT NULL,
    national_id text,
    bank_name text,
    bank_branch text,
    branch_code text,
    account_no text,
    account_name text,
    signature_svg text,
    submitted_at timestamp with time zone DEFAULT now() NOT NULL,
    status public.doc_status DEFAULT 'submitted'::public.doc_status NOT NULL,
    reviewed_at timestamp with time zone,
    reviewed_by uuid,
    reject_reason text,
    prefix text,
    CONSTRAINT ta_profile_submissions_prefix_check CHECK (((prefix IS NULL) OR (prefix = ANY (ARRAY['นาย'::text, 'นาง'::text, 'นางสาว'::text]))))
);


ALTER TABLE public.ta_profile_submissions OWNER TO itii_database_prod;

--
-- Name: ta_profiles; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.ta_profiles (
    user_id uuid NOT NULL,
    national_id text,
    bank_name text,
    bank_branch text,
    branch_code text,
    account_no text,
    account_name text,
    signature_svg text,
    completed_at timestamp with time zone,
    verified_at timestamp with time zone,
    verified_by uuid,
    reject_reason text,
    status public.doc_status DEFAULT 'pending'::public.doc_status NOT NULL,
    signature_png_b64 text,
    current_round integer DEFAULT 1 NOT NULL,
    prefix text,
    CONSTRAINT ta_profiles_prefix_check CHECK (((prefix IS NULL) OR (prefix = ANY (ARRAY['นาย'::text, 'นาง'::text, 'นางสาว'::text]))))
);


ALTER TABLE public.ta_profiles OWNER TO itii_database_prod;

--
-- Name: ta_request_assignments; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.ta_request_assignments (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    request_id uuid NOT NULL,
    section_id uuid NOT NULL,
    ta_id uuid NOT NULL,
    level public.study_level NOT NULL
);


ALTER TABLE public.ta_request_assignments OWNER TO itii_database_prod;

--
-- Name: ta_request_counts; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.ta_request_counts (
    request_id uuid NOT NULL,
    section_id uuid NOT NULL,
    undergrad_count integer DEFAULT 0 NOT NULL,
    graduate_count integer DEFAULT 0 NOT NULL
);


ALTER TABLE public.ta_request_counts OWNER TO itii_database_prod;

--
-- Name: ta_request_windows; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.ta_request_windows (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    term_id uuid NOT NULL,
    opens_at timestamp with time zone NOT NULL,
    closes_at timestamp with time zone NOT NULL,
    is_open boolean DEFAULT true NOT NULL,
    note text
);


ALTER TABLE public.ta_request_windows OWNER TO itii_database_prod;

--
-- Name: ta_requests; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.ta_requests (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    teaching_course_id uuid NOT NULL,
    window_id uuid,
    lecturer_id uuid NOT NULL,
    reimburse_scope public.reimburse_scope NOT NULL,
    status public.ta_request_status DEFAULT 'draft'::public.ta_request_status NOT NULL,
    submitted_at timestamp with time zone,
    decided_at timestamp with time zone,
    decided_by uuid,
    reject_reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    decision_checks jsonb DEFAULT '[]'::jsonb NOT NULL,
    is_late boolean DEFAULT false NOT NULL
);


ALTER TABLE public.ta_requests OWNER TO itii_database_prod;

--
-- Name: ta_review_schedules; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.ta_review_schedules (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    assignment_id uuid NOT NULL,
    day_of_week integer NOT NULL,
    start_time time without time zone NOT NULL,
    end_time time without time zone NOT NULL,
    room text,
    note text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ta_review_schedules_check CHECK ((end_time > start_time)),
    CONSTRAINT ta_review_schedules_day_of_week_check CHECK (((day_of_week >= 0) AND (day_of_week <= 6)))
);


ALTER TABLE public.ta_review_schedules OWNER TO itii_database_prod;

--
-- Name: ta_workload_forms; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.ta_workload_forms (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    assignment_id uuid NOT NULL,
    help_teach_hrs numeric(4,2) DEFAULT 0,
    help_teach_desc text,
    prep_hrs numeric(4,2) DEFAULT 0,
    prep_desc text,
    grade_hrs numeric(4,2) DEFAULT 0,
    grade_desc text,
    other_hrs numeric(4,2) DEFAULT 0,
    other_desc text,
    check_work_hrs numeric(4,2) DEFAULT 0,
    attendance_hrs numeric(4,2) DEFAULT 0,
    ug_other_hrs numeric(4,2) DEFAULT 0,
    ug_other_desc text,
    lab_hrs numeric(4,2) DEFAULT 0
);


ALTER TABLE public.ta_workload_forms OWNER TO itii_database_prod;

--
-- Name: teaching_courses; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.teaching_courses (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    term_id uuid NOT NULL,
    starts_on date,
    ends_on date,
    num_students integer DEFAULT 0 NOT NULL,
    created_by uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    num_students_regular integer DEFAULT 0 NOT NULL,
    num_students_special integer DEFAULT 0 NOT NULL,
    exported_at timestamp with time zone,
    code text NOT NULL,
    name_th text NOT NULL,
    name_en text,
    level text DEFAULT 'undergrad'::text NOT NULL,
    credits integer DEFAULT 0 NOT NULL,
    lecture_hrs integer DEFAULT 0 NOT NULL,
    lab_hrs integer DEFAULT 0 NOT NULL,
    self_hrs integer DEFAULT 0 NOT NULL,
    department text,
    CONSTRAINT teaching_courses_level_check CHECK ((level = ANY (ARRAY['undergrad'::text, 'graduate'::text])))
);


ALTER TABLE public.teaching_courses OWNER TO itii_database_prod;

--
-- Name: teaching_lecturers; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.teaching_lecturers (
    teaching_course_id uuid NOT NULL,
    lecturer_id uuid NOT NULL,
    is_primary boolean DEFAULT false NOT NULL
);


ALTER TABLE public.teaching_lecturers OWNER TO itii_database_prod;

--
-- Name: user_roles; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.user_roles (
    user_id uuid NOT NULL,
    role public.role_code NOT NULL
);


ALTER TABLE public.user_roles OWNER TO itii_database_prod;

--
-- Name: users; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.users (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    email public.citext NOT NULL,
    first_name text NOT NULL,
    last_name text NOT NULL,
    phone text,
    password_hash text,
    sso_subject text,
    study_level public.study_level,
    student_id text,
    department text,
    is_active boolean DEFAULT true NOT NULL,
    profile_completed boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    title text,
    must_change_password boolean DEFAULT false NOT NULL,
    study_year smallint,
    CONSTRAINT users_study_year_check CHECK (((study_year IS NULL) OR ((study_year >= 1) AND (study_year <= 8))))
);


ALTER TABLE public.users OWNER TO itii_database_prod;

--
-- Name: work_logs; Type: TABLE; Schema: public; Owner: itii_database_prod
--

CREATE TABLE public.work_logs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    assignment_id uuid NOT NULL,
    work_date date NOT NULL,
    start_time time without time zone NOT NULL,
    end_time time without time zone NOT NULL,
    hours numeric(4,2) NOT NULL,
    activity text NOT NULL,
    room text,
    note text,
    status public.worklog_status DEFAULT 'draft'::public.worklog_status NOT NULL,
    submitted_at timestamp with time zone,
    approved_at timestamp with time zone,
    approved_by uuid,
    reject_reason text,
    parent_kind text,
    CONSTRAINT work_logs_other_needs_parent_kind CHECK (((activity <> 'other'::text) OR (parent_kind = ANY (ARRAY['lecture'::text, 'lab'::text])))),
    CONSTRAINT work_logs_parent_kind_check CHECK (((parent_kind IS NULL) OR (parent_kind = ANY (ARRAY['lecture'::text, 'lab'::text]))))
);


ALTER TABLE public.work_logs OWNER TO itii_database_prod;

--
-- Name: audit_logs id; Type: DEFAULT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.audit_logs ALTER COLUMN id SET DEFAULT nextval('public.audit_logs_id_seq'::regclass);


--
-- Data for Name: academic_terms; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.academic_terms (id, academic_year, semester, starts_on, ends_on, is_active, months, midterm_starts_on, midterm_ends_on, final_starts_on, final_ends_on) FROM stdin;
2a01f439-a013-4f5f-a819-5ef591497243	2569	1	2026-06-22	2026-11-03	t	4	\N	\N	\N	\N
663670dd-25ac-4003-899b-8c6c357c39e3	2568	2	2025-11-12	2026-03-27	f	4	\N	\N	\N	\N
\.


--
-- Data for Name: admin_officers; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.admin_officers (id, academic_prefix, full_name, title, is_active, created_at, updated_at) FROM stdin;
4a8f4816-2391-454f-b4ce-390b8298d82a	ดร.	ศรัณย์ อภิชนตระกูล	รองคณบดีฝ่ายบริหาร	t	2026-07-10 02:29:53.716868+07	2026-07-10 02:29:53.716868+07
9c039090-1438-4b01-b39f-44a993596f79		ไพรวัลย์ คุณาสถิตย์ชัย	ผู้อำนวยการกองบริหารงานวิทยาลัย	t	2026-07-10 02:31:28.780239+07	2026-07-10 02:31:28.780239+07
66168307-cf01-44e4-b7ce-98909f41b67e	รองศาสตราจารย์ ดร.	สิรภัทร เชี่ยวชาญวัฒนา	คณบดีวิทยาลัยการคอมพิวเตอร์	t	2026-07-10 02:29:11.120067+07	2026-07-10 02:29:11.120067+07
77007a53-54e1-4a9b-a2cd-522463cbad91	ผู้ช่วยศาสตราจารย์ ดร.	ณกร วัฒนกิจ	รองคณบดีฝ่ายวิชาการ	t	2026-07-10 02:30:25.716031+07	2026-07-10 02:30:25.716031+07
e91c3dcd-16cb-4c9e-8367-a85713e8490f	รองศาสตราจารย์ ดร.	ชานนท์ เดชสุภา	รองคณบดีฝ่ายวิจัยและนวัตกรรม	t	2026-07-10 02:30:51.330046+07	2026-07-10 02:30:51.330046+07
e79b442e-615f-43c7-b052-54800ce11ec7	ผู้ช่วยศาสตราจารย์ ดร.	สาธิต กระเวนกิจ	ผู้ช่วยคณบดีฝ่ายดิจิทัล	t	2026-07-10 02:31:52.750024+07	2026-07-10 02:31:52.750024+07
5066c05f-827c-4c70-8a27-87486b2982c5	ผู้ช่วยศาสตราจารย์ ดร.	ไอศูรย์ กาญจนสุรัตน์	ผู้ช่วยคณบดีฝ่ายแผนและประกันคุณภาพ	t	2026-07-10 02:32:20.442114+07	2026-07-10 02:32:20.442114+07
0ca551b5-f2d0-418c-986a-767ec76afc8b	ผู้ช่วยศาสตราจารย์ ดร.	วรัญญา วรรณศรี	หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์	t	2026-07-10 02:34:50.53939+07	2026-07-10 02:34:50.53939+07
\.


--
-- Data for Name: announcements; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.announcements (id, title, body, audience, published_at, created_by, created_at, updated_at, category, pinned, cover_image_key, expires_at, announced_at, updated_by) FROM stdin;
\.


--
-- Data for Name: audit_logs; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.audit_logs (id, at, actor_id, actor_role, action, entity, entity_id, ip, user_agent, before, after, note) FROM stdin;
1	2026-07-08 01:58:54.180804+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36	\N	\N	\N
2	2026-07-08 02:48:30.940803+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36	\N	\N	\N
3	2026-07-08 03:41:32.181435+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	pay_rate.create	pay_rate	c15e2e2a-4910-48fb-a30b-5dd468c5c08d	\N	\N	\N	{"id": "c15e2e2a-4910-48fb-a30b-5dd468c5c08d", "note": "seed defaults from Excel workbook tab 2_59 ป.ตรี", "term_months": 4, "effective_from": "2026-07-07", "graduate_regular": 50, "undergrad_regular": 40, "undergrad_special": 50, "baseline_students_lab": 30, "ug_lab_hours_per_credit": 4.5, "graduate_special_lumpsum": 4000, "ug_workload_rate_regular": 200, "ug_workload_rate_special": 250, "baseline_students_lecture": 60, "ug_lecture_hours_per_credit": 3}	\N
4	2026-07-08 05:18:38.127039+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	hour_cap.upsert	hour_cap	b9d4adfc-87eb-4f26-9780-93066524046b	\N	\N	\N	{"id": "b9d4adfc-87eb-4f26-9780-93066524046b", "credits": 5, "hours_cap": 50}	\N
5	2026-07-08 05:19:00.861175+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	hour_cap.upsert	hour_cap	325e6402-e263-43d4-8803-ccaeb1b588b3	\N	\N	\N	{"id": "325e6402-e263-43d4-8803-ccaeb1b588b3", "credits": 5, "hours_cap": 55}	\N
6	2026-07-08 05:22:32.322181+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	hour_cap.delete	hour_cap	credits=5	\N	\N	\N	\N	\N
7	2026-07-08 05:27:13.117044+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	777ce205-a163-4cc9-9be9-cbd04cdda275	\N	\N	\N	{"id": "777ce205-a163-4cc9-9be9-cbd04cdda275", "code": "CP321007", "credits": 3, "lab_hrs": 0, "name_th": "การคิดเชิงออกแบบสาหรับเทคโนโลยีสารสนเทศ", "self_hrs": 6, "is_active": true, "lecture_hrs": 3}	\N
8	2026-07-08 05:49:22.781522+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	ab9a68f0-dfc0-48b0-b719-5b6ef8c718af	\N	\N	\N	{"id": "ab9a68f0-dfc0-48b0-b719-5b6ef8c718af", "code": "CP321001", "credits": 2, "lab_hrs": 2, "name_th": "การสร้างพอร์ตอาชีพด้านเทคโนโลยีสารสนเทศและจริยธรรมปัญญาประดิษฐ์", "self_hrs": 1, "is_active": true, "lecture_hrs": 0}	\N
9	2026-07-08 05:57:31.918018+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	9ee13653-b4d7-4b19-84c9-1624a34db412	\N	\N	\N	{"id": "9ee13653-b4d7-4b19-84c9-1624a34db412", "code": "CP322006", "credits": 3, "lab_hrs": 2, "name_th": "ความมั่นคงเทคโนโลยีสารสนเทศและการสื่อสาร", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
10	2026-07-08 05:58:26.908101+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	d291359d-dce8-4497-8d43-f0d800996881	\N	\N	\N	{"id": "d291359d-dce8-4497-8d43-f0d800996881", "code": "CP323002", "credits": 3, "lab_hrs": 0, "name_th": "การจัดการเชิงกลยุทธ์เทคโนโลยีสารสนเทศ", "self_hrs": 6, "is_active": true, "lecture_hrs": 3}	\N
11	2026-07-08 05:59:09.099151+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	8724bad8-0cf2-4285-82e5-37ba1d78263d	\N	\N	\N	{"id": "8724bad8-0cf2-4285-82e5-37ba1d78263d", "code": "CP323762", "credits": 3, "lab_hrs": 0, "name_th": "ระเบียบวิธีวิจัยและการสร้างนวัตกรรม", "self_hrs": 6, "is_active": true, "lecture_hrs": 3}	\N
12	2026-07-08 06:29:52.446767+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	term.upsert	term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"id": "2a01f439-a013-4f5f-a819-5ef591497243", "months": 4, "ends_on": "2026-11-03", "semester": 1, "is_active": true, "starts_on": "2026-06-22", "academic_year": 2569}	\N
13	2026-07-08 07:19:36.527518+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	term.create	term	663670dd-25ac-4003-899b-8c6c357c39e3	\N	\N	\N	{"id": "663670dd-25ac-4003-899b-8c6c357c39e3", "months": 4, "ends_on": "2026-03-27", "semester": 2, "is_active": false, "starts_on": "2025-11-12", "academic_year": 2568}	\N
14	2026-07-08 07:52:39.534983+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.create	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N	\N	{"email": "jakkritk@kku.ac.th", "roles": ["lecturer"], "title": "ดร.", "last_name": "แก้วโยธา", "first_name": "จักรกฤษณ์"}	\N
15	2026-07-08 07:57:25.358759+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.update	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N	\N	{"email": "jakkritk@kku.ac.th", "phone": "", "roles": ["lecturer"], "title": "อ. ดร.", "bank_name": "", "last_name": "แก้วโยธา", "account_no": "", "first_name": "จักรกฤษณ์", "bank_branch": "", "branch_code": "", "study_level": ""}	\N
16	2026-07-08 07:58:00.416703+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
17	2026-07-08 08:51:54.489477+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	0c367ed3-f276-4e81-bbc5-6f3a2ed3082c	\N	\N	\N	{"id": "0c367ed3-f276-4e81-bbc5-6f3a2ed3082c", "code": "CP323204", "credits": 3, "lab_hrs": 2, "name_th": "การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
18	2026-07-08 08:53:17.43671+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	teaching_course.create	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"ends_on": "2026-11-03", "term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": null, "starts_on": "2026-06-22", "lecturer_ids": null, "num_students": 0, "faculty_course_id": "0c367ed3-f276-4e81-bbc5-6f3a2ed3082c"}	\N
19	2026-07-08 09:14:20.439933+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
20	2026-07-08 09:28:55.498997+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	section.add	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"track": "regular", "sec_no": "1", "num_students": 0}	\N
21	2026-07-08 09:42:39.584072+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	section.update	section	13a54f98-c08c-453e-adcc-dc9971b07ba1	\N	\N	\N	{"num_students": 40}	\N
22	2026-07-08 09:42:56.833787+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	section.add	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"track": "special", "sec_no": "2", "num_students": 23}	\N
23	2026-07-08 10:23:12.194088+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	user.create	user	b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	{"email": "supphitan.p@kkumail.com", "roles": ["ta"], "title": "นาย", "last_name": "ภักสวัสดิ์", "first_name": "สุพพิธาน", "study_level": "undergrad"}	\N
24	2026-07-08 10:37:27.828409+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
25	2026-07-08 18:55:03.230949+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36	\N	\N	\N
26	2026-07-08 19:07:34.799493+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
27	2026-07-09 07:04:39.224687+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_profile.submit	ta_profile	b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	{"round": 1}	\N
28	2026-07-09 07:11:00.675193+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
29	2026-07-09 07:47:51.553984+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_profile.submit	ta_profile	b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	{"round": 1}	\N
30	2026-07-09 09:21:30.77792+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_profile.submit	ta_profile	b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	{"round": 1}	\N
31	2026-07-09 09:24:16.978399+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_doc.upload	ta_document	c3ca4679-8ab3-4194-879f-39b464b95ebd	\N	\N	\N	{"kind": "creditor_form", "round": 1, "filename": "creditor_form_สุพพิธาน_ภักสวัสดิ์.pdf"}	\N
32	2026-07-09 09:40:44.980841+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_doc.upload	ta_document	e839f896-1e74-44e7-8d68-0ded9c7e515d	\N	\N	\N	{"kind": "national_id", "round": 1, "filename": "733499934_3640200259468202_4940892661214164506_n.jpg"}	\N
33	2026-07-09 09:43:57.77237+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_doc.upload	ta_document	b0e8d729-307c-464c-a8c1-bf2707db5f08	\N	\N	\N	{"kind": "bank_book", "round": 1, "filename": "733499934_3640200259468202_4940892661214164506_n.jpg"}	\N
34	2026-07-09 09:46:23.904809+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36	\N	\N	\N
35	2026-07-09 09:55:17.011984+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_doc.review	ta_document	c3ca4679-8ab3-4194-879f-39b464b95ebd	\N	\N	\N	{"status": "approved"}	\N
36	2026-07-09 09:55:19.292709+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_doc.review	ta_document	e839f896-1e74-44e7-8d68-0ded9c7e515d	\N	\N	\N	{"status": "approved"}	\N
37	2026-07-09 09:55:30.995757+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_doc.review	ta_document	b0e8d729-307c-464c-a8c1-bf2707db5f08	\N	\N	\N	{"reason": "ส่งผิดภาพ", "status": "rejected"}	\N
38	2026-07-09 10:06:27.711207+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_doc.upload	ta_document	3a6b798d-a81c-4737-844b-de4dd322b77a	\N	\N	\N	{"kind": "bank_book", "round": 2, "filename": "727800449_1544583450671199_713885289005163738_n.jpg"}	\N
39	2026-07-09 10:06:46.297362+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_doc.review	ta_document	3a6b798d-a81c-4737-844b-de4dd322b77a	\N	\N	\N	{"status": "approved"}	\N
40	2026-07-09 10:06:49.333378+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_profile.review	ta_profile	b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	{"round": 1, "reason": "", "status": "approved"}	\N
41	2026-07-09 18:50:29.004985+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
42	2026-07-09 19:10:45.174519+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.create	user	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	\N	\N	{"email": "worapoj_s@kkumail.com", "roles": ["ta"], "title": "นาย", "last_name": "สุวรรณภิภพ", "first_name": "วรพจน์", "study_level": "phd"}	\N
43	2026-07-09 19:11:35.651468+07	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	auth.login	user	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36	\N	\N	\N
44	2026-07-09 19:14:25.69551+07	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	ta_profile.submit	ta_profile	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	\N	\N	{"round": 1}	\N
45	2026-07-09 19:14:34.127249+07	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	ta_doc.upload	ta_document	d3768244-84cd-4e3f-84d5-dc20d55f7d58	\N	\N	\N	{"kind": "creditor_form", "round": 1, "filename": "creditor_form_วรพจน์_สุวรรณภิภพ.pdf"}	\N
46	2026-07-09 19:14:53.267394+07	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	ta_doc.upload	ta_document	9221344f-fe75-4d4b-8436-fe5551567575	\N	\N	\N	{"kind": "national_id", "round": 1, "filename": "images.jpg"}	\N
47	2026-07-09 19:15:42.401269+07	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	ta_doc.upload	ta_document	77b1ef5c-ddf0-43f4-8205-dc98ad371a89	\N	\N	\N	{"kind": "bank_book", "round": 1, "filename": "JanJingJing_1.jpg"}	\N
48	2026-07-09 19:16:31.282042+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_doc.review	ta_document	77b1ef5c-ddf0-43f4-8205-dc98ad371a89	\N	\N	\N	{"status": "approved"}	\N
49	2026-07-09 19:16:31.899187+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_doc.review	ta_document	9221344f-fe75-4d4b-8436-fe5551567575	\N	\N	\N	{"status": "approved"}	\N
50	2026-07-09 19:16:32.743992+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_doc.review	ta_document	d3768244-84cd-4e3f-84d5-dc20d55f7d58	\N	\N	\N	{"status": "approved"}	\N
51	2026-07-09 19:16:35.931751+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_profile.review	ta_profile	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	\N	\N	{"round": 1, "reason": "", "status": "approved"}	\N
52	2026-07-09 20:36:57.024452+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
53	2026-07-09 21:02:46.897849+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	section.schedules.replace	section	13a54f98-c08c-453e-adcc-dc9971b07ba1	\N	\N	\N	[{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "12:00", "start_time": "09:00", "day_of_week": 1}]	\N
54	2026-07-09 21:03:09.201185+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	section.schedules.replace	section	95f12def-b084-4e87-8532-530a6deeef70	\N	\N	\N	[{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "12:00", "start_time": "09:00", "day_of_week": 1}]	\N
55	2026-07-09 21:03:28.328331+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	section.schedules.replace	section	13a54f98-c08c-453e-adcc-dc9971b07ba1	\N	\N	\N	[{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "12:00", "start_time": "09:00", "day_of_week": 1}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "12:00", "start_time": "09:00", "day_of_week": 2}]	\N
56	2026-07-09 21:03:44.994203+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	section.schedules.replace	section	95f12def-b084-4e87-8532-530a6deeef70	\N	\N	\N	[{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "12:00", "start_time": "09:00", "day_of_week": 1}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "12:00", "start_time": "09:00", "day_of_week": 3}]	\N
57	2026-07-09 22:12:47.528958+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36	\N	\N	\N
58	2026-07-09 22:16:30.852111+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_window.upsert	ta_window	dca519a3-484b-4ab3-b9d0-ae9876f14f69	\N	\N	\N	{"id": "dca519a3-484b-4ab3-b9d0-ae9876f14f69", "is_open": true, "term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "opens_at": "2026-07-09T15:16:00Z", "closes_at": "2026-08-08T15:16:00Z"}	\N
59	2026-07-09 22:16:45.927299+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	ta_request.submit	ta_request	a83c369d-00d1-45d2-b9cf-cdada2eb8f96	\N	\N	\N	{"counts": [{"section_id": "13a54f98-c08c-453e-adcc-dc9971b07ba1", "graduate_count": 1, "undergrad_count": 1}, {"section_id": "95f12def-b084-4e87-8532-530a6deeef70", "graduate_count": 0, "undergrad_count": 0}], "assignments": [{"level": "phd", "ta_id": "afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3", "workload": {"lab_hrs": 0, "prep_hrs": 4, "grade_hrs": 2, "other_hrs": 2, "prep_desc": "เตรียมเอกสาร/เนื้อหา", "grade_desc": "ตรวจงานตามที่อาจารย์มอบหมาย", "other_desc": "ช่วยเช็คชื่อ", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 0, "check_work_hrs": 0, "help_teach_hrs": 4, "help_teach_desc": "ช่วยแนะนํา/สอนปฏิบัตินักศึกษาในคาบเรียน"}, "section_id": "13a54f98-c08c-453e-adcc-dc9971b07ba1"}, {"level": "undergrad", "ta_id": "b134e943-7410-44fd-883b-0b32f4a93b33", "workload": {"lab_hrs": 4, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 2, "check_work_hrs": 4, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_id": "13a54f98-c08c-453e-adcc-dc9971b07ba1"}], "reimburse_scope": "both", "teaching_course_id": "4415242d-ffdb-45ab-b1d2-b95fa9df1cc8"}	\N
60	2026-07-09 22:28:16.682472+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_request.approve	ta_request	a83c369d-00d1-45d2-b9cf-cdada2eb8f96	\N	\N	\N	\N	\N
61	2026-07-09 22:32:01.847211+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"regular": 0, "special": 23, "num_students": 23}	\N
62	2026-07-09 22:32:05.164349+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"regular": 0, "special": 3, "num_students": 3}	\N
63	2026-07-09 22:32:10.491324+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"regular": 0, "special": 0, "num_students": 0}	\N
64	2026-07-09 22:43:21.325331+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36	\N	\N	\N
65	2026-07-10 02:01:12.063413+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
66	2026-07-10 02:29:11.126062+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	admin_officer.create	admin_officer	66168307-cf01-44e4-b7ce-98909f41b67e	\N	\N	\N	{"id": "66168307-cf01-44e4-b7ce-98909f41b67e", "title": "คณบดีวิทยาลัยการคอมพิวเตอร์", "full_name": "สิรภัทร เชี่ยวชาญวัฒนา", "is_active": true, "academic_prefix": "รศ.ดร."}	\N
67	2026-07-10 02:29:53.722797+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	admin_officer.create	admin_officer	4a8f4816-2391-454f-b4ce-390b8298d82a	\N	\N	\N	{"id": "4a8f4816-2391-454f-b4ce-390b8298d82a", "title": "รองคณบดีฝ่ายบริหาร", "full_name": "ศรัณย์ อภิชนตระกูล", "is_active": true, "academic_prefix": "ดร."}	\N
68	2026-07-10 02:30:25.720932+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	admin_officer.create	admin_officer	77007a53-54e1-4a9b-a2cd-522463cbad91	\N	\N	\N	{"id": "77007a53-54e1-4a9b-a2cd-522463cbad91", "title": "รองคณบดีฝ่ายวิชาการ", "full_name": "ณกร วัฒนกิจ", "is_active": true, "academic_prefix": "ผศ.ดร."}	\N
69	2026-07-10 02:30:51.333557+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	admin_officer.create	admin_officer	e91c3dcd-16cb-4c9e-8367-a85713e8490f	\N	\N	\N	{"id": "e91c3dcd-16cb-4c9e-8367-a85713e8490f", "title": "รองคณบดีฝ่ายวิจัยและนวัตกรรม", "full_name": "ชานนท์ เดชสุภา", "is_active": true, "academic_prefix": "รศ.ดร."}	\N
70	2026-07-10 02:31:28.783575+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	admin_officer.create	admin_officer	9c039090-1438-4b01-b39f-44a993596f79	\N	\N	\N	{"id": "9c039090-1438-4b01-b39f-44a993596f79", "title": "ผู้อำนวยการกองบริหารงานวิทยาลัย", "full_name": "ไพรวัลย์ คุณาสถิตย์ชัย", "is_active": true, "academic_prefix": ""}	\N
71	2026-07-10 02:31:52.753395+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	admin_officer.create	admin_officer	e79b442e-615f-43c7-b052-54800ce11ec7	\N	\N	\N	{"id": "e79b442e-615f-43c7-b052-54800ce11ec7", "title": "ผู้ช่วยคณบดีฝ่ายดิจิทัล", "full_name": "สาธิต กระเวนกิจ", "is_active": true, "academic_prefix": "ผศ.ดร."}	\N
72	2026-07-10 02:32:20.445991+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	admin_officer.create	admin_officer	5066c05f-827c-4c70-8a27-87486b2982c5	\N	\N	\N	{"id": "5066c05f-827c-4c70-8a27-87486b2982c5", "title": "ผู้ช่วยคณบดีฝ่ายแผนและประกันคุณภาพ", "full_name": "ไอศูรย์ กาญจนสุรัตน์", "is_active": true, "academic_prefix": "ผศ.ดร."}	\N
73	2026-07-10 02:34:50.542471+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	admin_officer.create	admin_officer	0ca551b5-f2d0-418c-986a-767ec76afc8b	\N	\N	\N	{"id": "0ca551b5-f2d0-418c-986a-767ec76afc8b", "title": "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์", "full_name": "วรัญญา วรรณศรี", "is_active": true, "academic_prefix": "ผศ.ดร."}	\N
74	2026-07-10 03:04:15.632024+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
75	2026-07-10 03:08:26.419296+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (iPhone; CPU iPhone OS 26_5_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/150.0.7871.51 Mobile/15E148 Safari/604.1	\N	\N	\N
76	2026-07-10 03:49:10.869412+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
77	2026-07-10 10:06:51.571861+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
78	2026-07-10 10:08:26.69284+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36	\N	\N	\N
79	2026-07-10 10:18:04.244731+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5.2 Safari/605.1.15	\N	\N	\N
80	2026-07-10 10:23:18.678+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5.2 Safari/605.1.15	\N	\N	\N
81	2026-07-10 10:24:05.904234+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export.course	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5.2 Safari/605.1.15	\N	\N	\N
82	2026-07-10 10:24:50.635557+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Mobile/15E148 Safari/604.1	\N	\N	\N
83	2026-07-10 10:31:47.670906+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	pay_rate.create	pay_rate	0c938ba4-6292-4c2b-8b8a-97626862921c	\N	\N	\N	{"id": "0c938ba4-6292-4c2b-8b8a-97626862921c", "note": "seed defaults from Excel workbook tab 2_59 ป.ตรี", "term_months": 4, "effective_from": "2026-07-10", "graduate_regular": 50, "undergrad_regular": 40, "undergrad_special": 50, "ug_max_hours_per_day": 7, "baseline_students_lab": 30, "max_courses_per_student": 3, "ug_lab_hours_per_credit": 4.5, "graduate_special_lumpsum": 4000, "ug_workload_rate_regular": 200, "ug_workload_rate_special": 250, "baseline_students_lecture": 60, "ug_lecture_hours_per_credit": 3}	\N
84	2026-07-12 10:28:10.78993+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
85	2026-07-12 12:04:44.282354+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
86	2026-07-12 13:38:15.235719+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.create	user	c82ac668-6004-45f1-bcbb-f762610dadaf	\N	\N	\N	{"email": "isoonkan@kku.ac.th", "roles": ["lecturer"], "title": "ดร. ผศ.", "last_name": "กาญจนสุรัตน์", "first_name": "ไอศูรย์"}	\N
87	2026-07-12 13:39:24.210598+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.create	user	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	\N	\N	\N	{"email": "monlwa@kku.ac.th", "roles": ["lecturer"], "title": "ดร. ผศ.", "last_name": "วัฒนะ", "first_name": "มัลลิกา"}	\N
88	2026-07-12 13:40:22.021077+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.create	user	2abd04e1-b22d-43fa-af57-a7830de6a2ac	\N	\N	\N	{"email": "waruwu@kku.ac.th", "roles": ["lecturer"], "title": "ดร. ผศ.", "last_name": "วรรณศรี", "first_name": "วรัญญา"}	\N
89	2026-07-13 02:08:14.518569+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
90	2026-07-13 07:10:23.82305+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
91	2026-07-13 07:11:40.852119+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
92	2026-07-13 07:13:11.023037+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
93	2026-07-13 07:13:11.930736+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 2, "order_no": "6/2569"}	\N
94	2026-07-13 07:13:25.514592+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 2, "order_no": "6/2569"}	\N
95	2026-07-13 07:14:08.451631+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
96	2026-07-13 07:14:09.362793+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 2, "order_no": "6/2569"}	\N
97	2026-07-13 07:15:20.833105+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 2, "order_no": "7/2569"}	\N
98	2026-07-13 07:15:39.47741+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	worklog.staff_edit	work_log	a0cfedab-ddd1-4c7c-86e4-ba33e62d8e37	\N	\N	\N	{"id": "a0cfedab-ddd1-4c7c-86e4-ba33e62d8e37", "note": "staff test insert", "room": "CoCo-401", "hours": 2, "status": "", "activity": "lecture", "end_time": "11:00:00", "work_date": "2026-06-22", "start_time": "09:00:00", "assignment_id": "273f3713-22b5-4b28-b281-069701f6a462"}	\N
99	2026-07-13 07:16:03.640828+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	worklog.staff_delete	work_log	a0cfedab-ddd1-4c7c-86e4-ba33e62d8e37	\N	\N	\N	\N	\N
100	2026-07-13 07:16:36.849307+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export.course	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	127.0.0.1	curl/8.17.0	\N	\N	\N
101	2026-07-13 07:16:36.867558+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export_batch.record	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"id": "e61c6057-a53f-485e-b046-a1014da40990", "ta_count": 0, "file_name": "CP323204_20260713_071636.zip", "file_path": "CP323204_20260713_071636.zip", "total_baht": 0, "generated_at": "2026-07-13T00:16:36Z", "generated_by": "19ff049b-34d5-4e3e-be1e-1e78ed4e35ac", "teaching_course_id": "4415242d-ffdb-45ab-b1d2-b95fa9df1cc8"}	\N
102	2026-07-14 01:16:06.679036+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
103	2026-07-14 01:23:12.340558+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.create	user	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	\N	\N	{"email": "chuthamat.cha@kkumail.com", "roles": ["ta"], "title": "นางสาว", "last_name": "ชะรานันท์", "first_name": "จุฑามาศ", "study_level": "undergrad"}	\N
104	2026-07-14 01:23:35.771034+07	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	auth.login	user	67959d3a-87dc-476f-ab3e-0ce6c054a444	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
105	2026-07-14 01:26:10.801297+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
106	2026-07-14 01:26:33.141626+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.reset_password	user	b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	\N	\N
107	2026-07-14 01:26:45.021188+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	curl/8.17.0	\N	\N	\N
108	2026-07-14 01:28:00.245426+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
109	2026-07-14 01:28:00.875768+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.reset_password	user	b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	\N	\N
110	2026-07-14 01:28:16.052303+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.reset_password	user	b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	\N	\N
111	2026-07-14 01:28:16.750069+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	curl/8.17.0	\N	\N	\N
112	2026-07-14 01:35:05.004186+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
113	2026-07-14 01:35:05.336151+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.reset_password	user	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	\N	\N	\N	\N
114	2026-07-14 01:35:06.048751+07	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	auth.login	user	67959d3a-87dc-476f-ab3e-0ce6c054a444	127.0.0.1	curl/8.17.0	\N	\N	\N
115	2026-07-14 02:15:04.719842+07	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	ta_profile.submit	ta_profile	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	\N	\N	{"round": 1}	\N
116	2026-07-14 02:15:58.594285+07	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	ta_doc.upload	ta_document	f85dccda-af40-4e1b-8b03-1d30a6084f10	\N	\N	\N	{"kind": "creditor_form", "round": 1, "filename": "creditor_form_จุฑามาศ_ชะรานันท์.pdf"}	\N
117	2026-07-14 02:16:07.051988+07	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	ta_doc.upload	ta_document	dc46188f-67fa-48b3-9be0-906b74a21eed	\N	\N	\N	{"kind": "national_id", "round": 1, "filename": "733499934_3640200259468202_4940892661214164506_n.jpg"}	\N
118	2026-07-14 02:16:17.669929+07	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	ta_doc.upload	ta_document	fbfb19fc-ff2f-4fda-94f9-305a5f8c5b8d	\N	\N	\N	{"kind": "bank_book", "round": 1, "filename": "733499934_3640200259468202_4940892661214164506_n.jpg"}	\N
119	2026-07-14 02:18:21.264972+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.create	user	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	\N	\N	\N	{"email": "thanadet.w@kkumail.com", "roles": ["ta"], "title": "นาย", "last_name": "วาตรีบุญเรือง", "first_name": "ธนเดช", "study_level": "undergrad"}	\N
120	2026-07-14 02:18:43.493709+07	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	\N	auth.login	user	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
121	2026-07-14 02:38:24.743741+07	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	\N	ta_profile.submit	ta_profile	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	\N	\N	\N	{"round": 1}	\N
122	2026-07-14 02:38:57.804381+07	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	\N	ta_doc.upload	ta_document	06babb4b-f242-47dc-8537-95fedccd6b1d	\N	\N	\N	{"kind": "creditor_form", "round": 1, "filename": "creditor_form_ธนเดช_วาตรีบุญเรือง.pdf"}	\N
123	2026-07-14 02:39:16.897898+07	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	\N	ta_doc.upload	ta_document	410f8e9c-8fe2-4ab1-b837-bce68b39a4a9	\N	\N	\N	{"kind": "bank_book", "round": 1, "filename": "733499934_3640200259468202_4940892661214164506_n.jpg"}	\N
124	2026-07-14 02:39:19.394118+07	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	\N	ta_doc.upload	ta_document	f6bca7f2-89cf-4ca6-9ae4-e95ec6adab3b	\N	\N	\N	{"kind": "national_id", "round": 1, "filename": "733499934_3640200259468202_4940892661214164506_n.jpg"}	\N
125	2026-07-14 02:42:48.325232+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
126	2026-07-14 03:15:39.299738+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	02931ca7-7569-429e-907a-b9b013a07272	\N	\N	\N	{"id": "02931ca7-7569-429e-907a-b9b013a07272", "code": "SC361002", "credits": 3, "lab_hrs": 2, "name_th": "การเขียนโปรแกรมเชิงโครงสร้างสำหรับเทคโนโลยีสารสนเทศ", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
127	2026-07-14 03:17:22.673406+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	5f5ac4c3-ef9b-4409-b620-7b9e70b9dcbb	\N	\N	\N	{"id": "5f5ac4c3-ef9b-4409-b620-7b9e70b9dcbb", "code": "CP421025", "credits": 3, "lab_hrs": 2, "name_th": "การวิเคราะห์และออกแบบซอฟต์แวร์", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
128	2026-07-14 04:44:53.660503+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	teaching_course.create	teaching_course	1c0b6bb3-87ec-43ff-9b63-ff0a491ad4d7	\N	\N	\N	{"ends_on": "2026-10-22", "term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [{"track": "regular", "sec_no": "1", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "10:30", "start_time": "08:30", "day_of_week": 5}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "12:30", "start_time": "10:30", "day_of_week": 5}], "num_students": 0}, {"track": "special", "sec_no": "2", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "17:00", "start_time": "15:00", "day_of_week": 4}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "19:00", "start_time": "17:00", "day_of_week": 4}], "num_students": 0}], "starts_on": "2026-06-22", "lecturer_ids": null, "num_students": 0, "faculty_course_id": "5f5ac4c3-ef9b-4409-b620-7b9e70b9dcbb"}	\N
129	2026-07-14 05:32:14.507154+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	ta_request.auto_decide	ta_request	5c01f193-e90e-44e4-ba3f-070a9f5fd75a	\N	\N	\N	{"counts": [{"section_id": "aeb5e366-7da6-4286-a823-c878c1c903fb", "graduate_count": 1, "undergrad_count": 2}, {"section_id": "db603f69-2872-48a1-8d7f-a9dafebbd0dd", "graduate_count": 1, "undergrad_count": 2}], "assignments": [{"level": "undergrad", "ta_id": "b134e943-7410-44fd-883b-0b32f4a93b33", "workload": {"lab_hrs": 4, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 0, "ug_other_desc": "ตรวจแต่งกาย", "attendance_hrs": 2, "check_work_hrs": 4, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_ids": ["aeb5e366-7da6-4286-a823-c878c1c903fb", "db603f69-2872-48a1-8d7f-a9dafebbd0dd"]}, {"level": "phd", "ta_id": "afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3", "workload": {"lab_hrs": 0, "prep_hrs": 4, "grade_hrs": 2, "other_hrs": 2, "prep_desc": "เตรียมเอกสาร/เนื้อหา", "grade_desc": "ตรวจงานตามที่อาจารย์มอบหมาย", "other_desc": "ช่วยเช็คชื่อ", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 0, "check_work_hrs": 0, "help_teach_hrs": 4, "help_teach_desc": "ช่วยแนะนํา/สอนปฏิบัตินักศึกษาในคาบเรียน"}, "section_ids": ["aeb5e366-7da6-4286-a823-c878c1c903fb", "db603f69-2872-48a1-8d7f-a9dafebbd0dd"]}, {"level": "undergrad", "ta_id": "67959d3a-87dc-476f-ab3e-0ce6c054a444", "workload": {"lab_hrs": 2, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 2, "check_work_hrs": 2, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_ids": ["aeb5e366-7da6-4286-a823-c878c1c903fb"]}, {"level": "undergrad", "ta_id": "1a7a52ee-435e-40f9-b9ff-dad6725ecc7c", "workload": {"lab_hrs": 2, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 2, "check_work_hrs": 2, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_ids": ["db603f69-2872-48a1-8d7f-a9dafebbd0dd"]}], "reimburse_scope": "both", "teaching_course_id": "1c0b6bb3-87ec-43ff-9b63-ff0a491ad4d7"}	rejected
130	2026-07-14 05:54:14.241214+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	ta_request.auto_decide	ta_request	73c55388-57e7-4def-b204-e588b8c28a54	\N	\N	\N	{"counts": [{"section_id": "aeb5e366-7da6-4286-a823-c878c1c903fb", "graduate_count": 1, "undergrad_count": 1}, {"section_id": "db603f69-2872-48a1-8d7f-a9dafebbd0dd", "graduate_count": 1, "undergrad_count": 3}], "assignments": [{"level": "undergrad", "ta_id": "67959d3a-87dc-476f-ab3e-0ce6c054a444", "workload": {"lab_hrs": 4, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 2, "check_work_hrs": 4, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_ids": ["aeb5e366-7da6-4286-a823-c878c1c903fb", "db603f69-2872-48a1-8d7f-a9dafebbd0dd"]}, {"level": "undergrad", "ta_id": "1a7a52ee-435e-40f9-b9ff-dad6725ecc7c", "workload": {"lab_hrs": 2, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 2, "check_work_hrs": 2, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_ids": ["db603f69-2872-48a1-8d7f-a9dafebbd0dd"]}, {"level": "phd", "ta_id": "afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3", "workload": {"lab_hrs": 0, "prep_hrs": 4, "grade_hrs": 2, "other_hrs": 2, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 0, "check_work_hrs": 0, "help_teach_hrs": 4, "help_teach_desc": ""}, "section_ids": ["aeb5e366-7da6-4286-a823-c878c1c903fb", "db603f69-2872-48a1-8d7f-a9dafebbd0dd"]}, {"level": "undergrad", "ta_id": "b134e943-7410-44fd-883b-0b32f4a93b33", "workload": {"lab_hrs": 2, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 2, "check_work_hrs": 2, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_ids": ["db603f69-2872-48a1-8d7f-a9dafebbd0dd"]}], "reimburse_scope": "both", "teaching_course_id": "1c0b6bb3-87ec-43ff-9b63-ff0a491ad4d7"}	approved
131	2026-07-14 05:57:44.03743+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.reset_password	user	b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	\N	\N
132	2026-07-14 05:57:51.146343+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
133	2026-07-14 09:57:09.200989+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	fc93c0e2-e7ca-43e1-94e1-4416f7a420d7	\N	\N	\N	{"id": "fc93c0e2-e7ca-43e1-94e1-4416f7a420d7", "code": "SC363001", "credits": 3, "lab_hrs": 2, "name_th": "การวิเคราะห์และออกแบบระบบ", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
134	2026-07-14 10:05:50.219367+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	teaching_course.create	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [{"track": "regular", "sec_no": "1", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "12:30", "start_time": "10:30", "day_of_week": 2}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "15:00", "start_time": "13:00", "day_of_week": 2}], "num_students": 0}], "lecturer_ids": null, "num_students": 0, "faculty_course_id": "fc93c0e2-e7ca-43e1-94e1-4416f7a420d7"}	\N
135	2026-07-14 10:08:01.998592+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
136	2026-07-14 10:10:33.755112+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	section.schedules.replace	section	47c9d7aa-35f5-4f4c-a833-2d8ea3593c48	\N	\N	\N	[{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "12:30", "start_time": "10:30", "day_of_week": 4}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "15:00", "start_time": "13:00", "day_of_week": 4}]	\N
137	2026-07-14 10:11:09.186113+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	ta_request.auto_decide	ta_request	78226706-5d19-4c03-9369-359d1f814777	\N	\N	\N	{"counts": [{"section_id": "47c9d7aa-35f5-4f4c-a833-2d8ea3593c48", "graduate_count": 0, "undergrad_count": 1}], "assignments": [{"level": "undergrad", "ta_id": "b134e943-7410-44fd-883b-0b32f4a93b33", "workload": {"lab_hrs": 2, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 1, "ug_other_desc": "", "attendance_hrs": 2, "check_work_hrs": 2, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_ids": ["47c9d7aa-35f5-4f4c-a833-2d8ea3593c48"]}], "reimburse_scope": "both", "teaching_course_id": "a416b931-2506-45a8-96c7-d06de9246ff7"}	approved
138	2026-07-15 04:41:51.755987+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
139	2026-07-15 04:43:00.680745+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
140	2026-07-15 06:36:28.087184+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.generate	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	{"count": 0}	\N
141	2026-07-15 06:36:50.165913+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.generate	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	{"count": 0}	\N
142	2026-07-15 06:40:36.740545+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.generate	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	{"count": 0}	\N
143	2026-07-15 06:41:15.597056+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.generate	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	{"count": 0}	\N
144	2026-07-15 06:47:41.66297+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
145	2026-07-15 07:18:59.959636+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_review_schedule.add	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	{"note": "", "room": "", "end_time": "16:00", "start_time": "14:00", "day_of_week": 6}	\N
146	2026-07-15 07:19:08.24187+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.generate	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	{"count": 0}	\N
147	2026-07-15 07:27:26.246404+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
148	2026-07-15 08:00:29.67603+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
149	2026-07-15 08:04:09.299521+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	holiday.sync_bot	holiday	\N	\N	\N	\N	\N	year=2026 fetched=19 inserted=14 updated=0 skipped=5
150	2026-07-15 08:25:29.688644+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.generate	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	{"count": 57}	\N
151	2026-07-15 08:50:42.08834+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	70a2aeff-48c3-4292-9de4-9e1d8a4f2071	\N	\N	\N	\N	\N
152	2026-07-15 08:50:42.813625+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	d763a74a-08e5-4e79-8131-b8c3fb85078b	\N	\N	\N	\N	\N
153	2026-07-15 08:50:42.829703+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	e31e4887-d0fb-40c9-be15-3afbf763a486	\N	\N	\N	\N	\N
154	2026-07-15 08:50:42.846624+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	0458e4ef-97ad-4781-bb9b-9cdfc3ae5472	\N	\N	\N	\N	\N
155	2026-07-15 08:50:42.86453+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	42b47b65-90ef-452a-973a-f9d71ddcde13	\N	\N	\N	\N	\N
156	2026-07-15 08:50:42.879749+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	ab7c7292-cea0-4103-abd7-4a9c310037b8	\N	\N	\N	\N	\N
157	2026-07-15 08:50:42.895437+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	d59ca10a-33e9-4b09-836e-4e70ff4559b2	\N	\N	\N	\N	\N
158	2026-07-15 08:50:42.911445+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	5cdfb72a-26ab-4ab1-92aa-75001f00115c	\N	\N	\N	\N	\N
159	2026-07-15 08:50:42.926225+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	1d16d676-3266-4798-a8a6-8d972b92ff3e	\N	\N	\N	\N	\N
160	2026-07-15 08:50:42.940363+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	55d0f8f1-893f-48f1-861a-deedfb645474	\N	\N	\N	\N	\N
161	2026-07-15 08:50:42.954229+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	6d5489f0-9ad7-4102-a769-9ad45a1dc55f	\N	\N	\N	\N	\N
162	2026-07-15 08:50:42.967186+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	ac6cbb9c-6c2f-4540-8c21-7ebf1cfe17f3	\N	\N	\N	\N	\N
163	2026-07-15 08:50:42.979953+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	202cb4c7-a2b4-4590-9601-0cb21c7424f2	\N	\N	\N	\N	\N
164	2026-07-15 08:50:42.993786+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	3c0b9b7f-5e42-4440-93e6-71d822078f41	\N	\N	\N	\N	\N
165	2026-07-15 08:50:43.006959+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	f521519b-e2a1-456c-85cd-ee39af27f42f	\N	\N	\N	\N	\N
166	2026-07-15 08:50:43.021944+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	60100473-9a60-4243-ac0d-f690d6062f41	\N	\N	\N	\N	\N
167	2026-07-15 08:50:43.035533+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	4d59f891-ce2a-413f-85f0-547d0cbd9e16	\N	\N	\N	\N	\N
168	2026-07-15 08:50:43.051029+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	12dd6283-c95a-4576-98c1-92c9587cd92c	\N	\N	\N	\N	\N
169	2026-07-15 08:50:43.064755+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	873fd3a0-ff69-4ccf-a9cc-7c16c8013ff4	\N	\N	\N	\N	\N
170	2026-07-15 08:50:43.079191+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	f548c044-9f28-4102-aeee-1298c7701ae5	\N	\N	\N	\N	\N
171	2026-07-15 08:50:43.092401+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	1fc26965-1559-4dcb-92d1-33c80d4ad92f	\N	\N	\N	\N	\N
172	2026-07-15 08:50:43.107159+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	ac6950c2-dc58-4f88-b4f6-ac5a4dedd5cc	\N	\N	\N	\N	\N
173	2026-07-15 08:50:43.120493+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	06b525d2-6850-4e15-85ee-2160c4710286	\N	\N	\N	\N	\N
174	2026-07-15 08:50:43.133912+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	a182593f-b3c7-4671-98c3-ee68f41f52a2	\N	\N	\N	\N	\N
175	2026-07-15 08:50:43.147935+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	2e3aa273-4b4a-48d1-9d04-49adedc73e27	\N	\N	\N	\N	\N
176	2026-07-15 08:50:43.161386+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	f7dd965c-bc2d-4eba-925c-887649dba9f7	\N	\N	\N	\N	\N
177	2026-07-15 08:50:43.176358+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	5be5ebf5-6f8e-4729-8f7f-4fa5de15dfd2	\N	\N	\N	\N	\N
178	2026-07-15 08:50:43.189635+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	c2f71496-8119-4272-99c9-5205d433938f	\N	\N	\N	\N	\N
179	2026-07-15 08:50:43.203796+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	709a2353-683f-44ac-a6e1-3435ba483e4f	\N	\N	\N	\N	\N
180	2026-07-15 08:50:43.217253+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	240cacaf-7b10-4133-a196-0e640cb07b0f	\N	\N	\N	\N	\N
181	2026-07-15 08:50:43.230846+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	73bd4e6f-1cf0-4cae-869d-7ebad32efa13	\N	\N	\N	\N	\N
182	2026-07-15 08:50:43.244712+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	ca0e5a38-072b-4d72-82d5-47202d2ea2f4	\N	\N	\N	\N	\N
183	2026-07-15 08:50:43.258169+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	73466643-ae26-41f1-ade4-afc1878de87f	\N	\N	\N	\N	\N
184	2026-07-15 08:50:43.274477+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	7885b42a-a684-4e7f-9ee1-a88ca072a6b4	\N	\N	\N	\N	\N
185	2026-07-15 08:50:43.288752+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	408d9b8a-4409-4025-8841-512afc2f38c0	\N	\N	\N	\N	\N
186	2026-07-15 08:50:43.3053+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	5e9b3c79-6b46-443c-945a-665b6927699c	\N	\N	\N	\N	\N
187	2026-07-15 08:50:43.323432+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	93aa03d8-515d-4399-9e9c-578c56609565	\N	\N	\N	\N	\N
188	2026-07-15 08:50:43.342093+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	b396df36-2bda-40f7-909c-e82f5aeae9cb	\N	\N	\N	\N	\N
189	2026-07-15 08:50:43.360434+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	7b0e196f-ea00-4e79-905e-8757e7c2016b	\N	\N	\N	\N	\N
190	2026-07-15 08:50:43.378582+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	ea4033da-6b20-4d7e-95ff-57ab77aae6da	\N	\N	\N	\N	\N
191	2026-07-15 08:50:43.39673+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	465c6278-4064-4695-abb3-3d691911790c	\N	\N	\N	\N	\N
192	2026-07-15 08:50:43.41502+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	244f2e83-93e9-48b0-a46f-9f577ce2dfc4	\N	\N	\N	\N	\N
193	2026-07-15 08:50:43.431387+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	83c88d2f-1069-4ce4-9007-bdfa6bac0e46	\N	\N	\N	\N	\N
194	2026-07-15 08:50:43.447498+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	493fe43b-b58e-46b2-8f8a-92757c83b8a4	\N	\N	\N	\N	\N
195	2026-07-15 08:50:43.463812+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	8386e6e2-6eda-4ee1-8461-51677bab9999	\N	\N	\N	\N	\N
196	2026-07-15 08:50:43.480728+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	fe726b13-f799-4c23-b4f4-8630a2385cf6	\N	\N	\N	\N	\N
197	2026-07-15 08:50:43.498473+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	76cb0a3d-c794-4801-a729-9d36cc701b9b	\N	\N	\N	\N	\N
198	2026-07-15 08:50:43.516121+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	fe5e608a-5239-4ab1-8f24-6fce13711f1a	\N	\N	\N	\N	\N
199	2026-07-15 08:50:43.53116+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	9ee5da5e-1a03-43f0-8c59-4c4bb0426c33	\N	\N	\N	\N	\N
200	2026-07-15 08:50:43.546151+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	94f001a9-7909-4493-accc-458ab79aac96	\N	\N	\N	\N	\N
201	2026-07-15 08:50:43.560209+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	b9a0e763-adaf-41ff-9de4-7314300a51a4	\N	\N	\N	\N	\N
202	2026-07-15 08:50:43.573975+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	b5d6dd70-a6cf-44a3-89e3-a06085382d0e	\N	\N	\N	\N	\N
203	2026-07-15 08:50:43.587043+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	f7aa7454-3920-404f-9b1c-921387a0ef9f	\N	\N	\N	\N	\N
204	2026-07-15 08:50:43.603119+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	cc6b5331-69c3-4f60-ae7b-3311e5eeed43	\N	\N	\N	\N	\N
205	2026-07-15 08:50:43.621408+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	100a6208-a5f1-44c2-9849-1ffb0097c95f	\N	\N	\N	\N	\N
206	2026-07-15 08:50:43.637952+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	4e8b6759-b10a-4d42-b7cd-1eaa98936151	\N	\N	\N	\N	\N
207	2026-07-15 08:50:43.65437+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	c26057d9-6dba-4bfb-9a09-3a62f84df47b	\N	\N	\N	\N	\N
208	2026-07-15 08:50:48.332407+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.generate	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	{"count": 57}	\N
209	2026-07-15 09:57:47.928692+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	075ec6fc-cf71-4b5a-ab20-ec975ea3399d	\N	\N	\N	\N	\N
210	2026-07-15 09:57:48.575551+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	aebac2a1-93f1-4a86-9718-24d9b73426df	\N	\N	\N	\N	\N
211	2026-07-15 09:57:48.587467+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	5690510e-bc1c-44bf-b195-0422c81f8939	\N	\N	\N	\N	\N
212	2026-07-15 09:57:48.606274+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	877e916c-5f74-4bee-b09f-b9cc34e8ac6b	\N	\N	\N	\N	\N
213	2026-07-15 09:57:48.622168+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	8a0b704f-b307-4b25-bc08-5f915d2e2b68	\N	\N	\N	\N	\N
214	2026-07-15 09:57:48.636862+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	7e1d69ff-c9d6-4143-91aa-80d3f218411c	\N	\N	\N	\N	\N
215	2026-07-15 09:57:48.650716+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	276ce38c-0352-47fb-8e88-4d4788fc61e5	\N	\N	\N	\N	\N
216	2026-07-15 09:57:48.668567+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	c9a438a1-2a02-4c58-9812-9392848e8083	\N	\N	\N	\N	\N
217	2026-07-15 09:57:48.680671+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	2df8232b-7f04-4e18-875d-3a2221569399	\N	\N	\N	\N	\N
218	2026-07-15 09:57:48.693103+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	05ae9fb2-a56b-4de0-92b5-626054e6413e	\N	\N	\N	\N	\N
219	2026-07-15 09:57:48.705032+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	015e2bee-4e0f-4608-a72a-35ee0d3f9a5c	\N	\N	\N	\N	\N
220	2026-07-15 09:57:48.717905+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	4f92b6a0-6b1b-4361-91e4-ff6eb9789864	\N	\N	\N	\N	\N
221	2026-07-15 09:57:48.732598+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	cddca646-802e-48c7-9b93-d113fbc5995d	\N	\N	\N	\N	\N
222	2026-07-15 09:57:48.746437+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	3bc8fa1c-0c53-481f-9976-d2e2f4c2e137	\N	\N	\N	\N	\N
223	2026-07-15 09:57:48.761313+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	9480b718-2b64-42ba-9ddc-9ef9fd62e94e	\N	\N	\N	\N	\N
224	2026-07-15 09:57:48.775963+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	b13ed46a-8bbf-4e0b-a636-e0e9c565273a	\N	\N	\N	\N	\N
225	2026-07-15 09:57:48.791005+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	b329a5e1-c99f-48e7-bbd8-80f64918831b	\N	\N	\N	\N	\N
226	2026-07-15 09:57:48.806043+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	abdb8606-4624-4f96-aa9c-0039b7986760	\N	\N	\N	\N	\N
227	2026-07-15 09:57:48.820268+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	7791e06d-6813-4b61-b629-952354bb4f6e	\N	\N	\N	\N	\N
228	2026-07-15 09:57:48.832616+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	33e32212-c1ed-4f49-8f50-f502a87da3bb	\N	\N	\N	\N	\N
229	2026-07-15 09:57:48.844601+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	157a06c5-ffb4-4a17-accc-db4ecdc5aa7d	\N	\N	\N	\N	\N
230	2026-07-15 09:57:48.856615+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	dd225484-1605-4117-8c4e-9107a6a68bd8	\N	\N	\N	\N	\N
231	2026-07-15 09:57:48.869689+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	544cf0c6-48a2-4796-8ce9-f18beb48e819	\N	\N	\N	\N	\N
232	2026-07-15 09:57:48.882439+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	25acfd2e-4798-46de-9536-2429f12a7aab	\N	\N	\N	\N	\N
233	2026-07-15 09:57:48.894133+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	25501a42-0086-47ca-bb4c-e0019d576012	\N	\N	\N	\N	\N
234	2026-07-15 09:57:48.906517+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	e09569b6-b10f-4c3a-af63-95fd28df2ec0	\N	\N	\N	\N	\N
235	2026-07-15 09:57:48.917954+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	3bd34605-16cb-47bd-bbc4-bc9670ddc444	\N	\N	\N	\N	\N
236	2026-07-15 09:57:48.932754+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	a39060a9-d661-4d2d-9415-f1911d3f2525	\N	\N	\N	\N	\N
237	2026-07-15 09:57:48.944446+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	2f86f6fb-834f-4de4-8932-05803bf2e285	\N	\N	\N	\N	\N
238	2026-07-15 09:57:48.955713+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	c94f63a4-114a-401d-8c60-aa1cecb8a2ad	\N	\N	\N	\N	\N
239	2026-07-15 09:57:48.967786+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	6faa98f4-6423-43fd-acde-3f6d378008e8	\N	\N	\N	\N	\N
240	2026-07-15 09:57:48.980334+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	81d76130-e2db-478a-98f4-ea065b1ee4a9	\N	\N	\N	\N	\N
241	2026-07-15 09:57:48.992934+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	4b9e3050-427c-4eba-99ee-1c4fd8abe2e5	\N	\N	\N	\N	\N
242	2026-07-15 09:57:49.005227+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	f25e88bc-66c5-4ced-8cf5-b48097ba68ec	\N	\N	\N	\N	\N
243	2026-07-15 09:57:49.017566+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	2cd28c2d-d424-45aa-ba9a-8fbbc742a7cd	\N	\N	\N	\N	\N
244	2026-07-15 09:57:49.030384+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	178a151b-be5b-4d7d-bbbe-eb12bffdfc81	\N	\N	\N	\N	\N
245	2026-07-15 09:57:49.043088+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	75a3dd4a-ad5b-4abe-8c00-2c5c50d9b9ff	\N	\N	\N	\N	\N
246	2026-07-15 09:57:49.055671+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	daf83920-6493-48bd-ba46-d24cf77db032	\N	\N	\N	\N	\N
247	2026-07-15 09:57:49.070035+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	dab3d3d8-df61-42ad-9346-00da6ea28a61	\N	\N	\N	\N	\N
248	2026-07-15 09:57:49.083013+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	28a09ca8-0199-496f-8e12-74f3927fbba2	\N	\N	\N	\N	\N
249	2026-07-15 09:57:49.096199+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	56791cd6-4a7b-4efc-8065-2a1c42a537c9	\N	\N	\N	\N	\N
250	2026-07-15 09:57:49.110685+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	01206ef6-e54a-4308-aae1-2c33f50ea302	\N	\N	\N	\N	\N
251	2026-07-15 09:57:49.123994+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	69471fcf-3cbf-482b-8c59-7a4bc781916d	\N	\N	\N	\N	\N
252	2026-07-15 09:57:49.13551+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	a955dfaa-fb12-43e2-b232-b41858df4401	\N	\N	\N	\N	\N
253	2026-07-15 09:57:49.147188+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	9be626c4-a446-4be3-80d5-1fe1d663db8f	\N	\N	\N	\N	\N
254	2026-07-15 09:57:49.159865+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	fd0c1c18-c931-4bc8-a81d-af455c1ceca6	\N	\N	\N	\N	\N
255	2026-07-15 09:57:49.173981+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	898bc826-3752-4871-97e8-ff96de42efa5	\N	\N	\N	\N	\N
256	2026-07-15 09:57:49.187227+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	29e5fe08-4e87-43cc-bdee-a162cd081d13	\N	\N	\N	\N	\N
257	2026-07-15 09:57:49.201407+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	89485a5d-1484-4c0d-8dc4-0f1d031d6bee	\N	\N	\N	\N	\N
258	2026-07-15 09:57:49.212287+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	76224427-b741-4815-bad3-42c047624c5e	\N	\N	\N	\N	\N
259	2026-07-15 09:57:49.225307+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	44062f01-0c98-4169-a9fc-70e18ce9b21e	\N	\N	\N	\N	\N
260	2026-07-15 09:57:49.236978+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	b9157355-4fc3-4e4e-8827-5d5f38173777	\N	\N	\N	\N	\N
261	2026-07-15 09:57:49.251297+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	c72614ad-039c-4f95-82c4-dfbb8171dd8a	\N	\N	\N	\N	\N
262	2026-07-15 09:57:49.26571+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	1abdf5ff-bc52-4582-b78c-6d2e7390201a	\N	\N	\N	\N	\N
263	2026-07-15 09:57:49.285956+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	036516f1-46a2-40f0-af80-34f20de0ae5d	\N	\N	\N	\N	\N
264	2026-07-15 09:57:49.298805+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	ae5591fa-12c2-4944-becc-43fe9e072541	\N	\N	\N	\N	\N
265	2026-07-15 09:57:49.317117+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	b442a68d-d154-47d8-a452-608a58d4348d	\N	\N	\N	\N	\N
266	2026-07-15 10:11:47.370845+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.generate	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	{"count": 57}	\N
267	2026-07-15 10:29:32.631613+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	3a3a4f8c-7991-4cea-bc4f-38cfae29b21c	\N	\N	\N	\N	\N
268	2026-07-15 10:29:33.202707+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	29971b62-1ad2-4778-804c-d45587abb724	\N	\N	\N	\N	\N
269	2026-07-15 10:29:33.217146+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	92944ef7-b25d-44e1-ab4f-9f2666956f0c	\N	\N	\N	\N	\N
270	2026-07-15 10:29:33.232238+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	b4722c43-ce6f-47d7-b1b0-efdece1008fe	\N	\N	\N	\N	\N
271	2026-07-15 10:29:33.247273+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	e068d776-76fc-4ba3-8872-43a8dcb4d427	\N	\N	\N	\N	\N
272	2026-07-15 10:29:33.261858+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	012c0c44-a382-4c4e-91da-3471a977c337	\N	\N	\N	\N	\N
273	2026-07-15 10:29:33.275992+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	f379aff4-67fe-4edc-9275-6e508c723435	\N	\N	\N	\N	\N
274	2026-07-15 10:29:33.290676+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	4b184e18-c5d5-4660-b8e0-9c6a96767ef5	\N	\N	\N	\N	\N
275	2026-07-15 10:29:33.305085+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	edf6e77a-5a8b-411f-b5f1-fc63c9b9e7da	\N	\N	\N	\N	\N
276	2026-07-15 10:29:33.321241+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	3d08476a-396b-4f4a-826e-8235b946eb34	\N	\N	\N	\N	\N
277	2026-07-15 10:29:33.335231+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	0369f7ce-3e89-4f1c-b2a4-2d874261b86f	\N	\N	\N	\N	\N
278	2026-07-15 10:29:33.349235+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	7aab9943-42f4-444e-908e-40f371f4f9af	\N	\N	\N	\N	\N
279	2026-07-15 10:29:33.36199+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	6781ba95-3873-4209-bda0-bc614e74ed5b	\N	\N	\N	\N	\N
280	2026-07-15 10:29:33.378406+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	fef5d49c-e984-4795-9f2c-cd9706c991b8	\N	\N	\N	\N	\N
281	2026-07-15 10:29:33.393003+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	b2e18f2d-3352-4dab-bddb-82ba4e6e2834	\N	\N	\N	\N	\N
282	2026-07-15 10:29:33.40858+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	700ab7dc-ad33-4e89-904f-d12f9d24e660	\N	\N	\N	\N	\N
283	2026-07-15 10:29:33.421821+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	d8d3fa77-ecb5-4ac2-914c-4327a8b1500d	\N	\N	\N	\N	\N
284	2026-07-15 10:29:33.437525+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	f728bafc-353f-458c-a51f-2f743cd0879f	\N	\N	\N	\N	\N
285	2026-07-15 10:29:33.455391+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	01929543-99e5-4330-a40e-f5e420dea0eb	\N	\N	\N	\N	\N
286	2026-07-15 10:29:33.474975+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	a0f4839d-7994-46b9-8f0b-be0ca9a31d53	\N	\N	\N	\N	\N
287	2026-07-15 10:29:33.494514+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	d272c339-f1d4-4a42-96ac-5cfdf4d7668c	\N	\N	\N	\N	\N
288	2026-07-15 10:29:33.514428+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	47cc177b-591a-4f35-930b-0c2f42c6d08b	\N	\N	\N	\N	\N
289	2026-07-15 10:29:33.534239+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	aded72c1-5da9-4760-83d2-2128c4688035	\N	\N	\N	\N	\N
290	2026-07-15 10:29:33.551392+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	529dc700-6c30-4c93-8ed1-1eb506cdaa60	\N	\N	\N	\N	\N
291	2026-07-15 10:29:33.568858+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	bc274c72-03f2-4c87-aee6-4bc9d2c0548b	\N	\N	\N	\N	\N
292	2026-07-15 10:29:33.586032+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	90952805-db61-4c0e-a034-ea9f995d47bd	\N	\N	\N	\N	\N
293	2026-07-15 10:29:33.604097+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	97d06f06-0c83-4f78-9572-4cb81aafabe4	\N	\N	\N	\N	\N
294	2026-07-15 10:29:33.618111+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	476ebeb4-5528-4ba8-8c82-150db98e1bc7	\N	\N	\N	\N	\N
295	2026-07-15 10:29:33.63117+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	80a87b41-4be9-40c8-9527-df58b4b85114	\N	\N	\N	\N	\N
296	2026-07-15 10:29:33.644898+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	cdd00800-90e9-45ea-a4f0-1def0c0922cc	\N	\N	\N	\N	\N
297	2026-07-15 10:29:33.661526+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	d92aaa2d-cdce-4c2c-b239-3121532d2b8e	\N	\N	\N	\N	\N
298	2026-07-15 10:29:33.675215+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	b53cde92-3084-47b7-97bb-5db8b8451bd3	\N	\N	\N	\N	\N
299	2026-07-15 10:29:33.689095+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	2e8ffd2e-b3c5-4fdf-b1c6-afb4e5c4f56a	\N	\N	\N	\N	\N
300	2026-07-15 10:29:33.702052+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	c6fca2d1-188a-4e3f-99d7-b11f86e63d53	\N	\N	\N	\N	\N
301	2026-07-15 10:29:33.717171+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	5a25d7b9-4a90-4b66-8941-1ab6a38d5edd	\N	\N	\N	\N	\N
302	2026-07-15 10:29:33.731276+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	76ca8696-4df0-459e-8592-c74aef93e1da	\N	\N	\N	\N	\N
303	2026-07-15 10:29:33.746339+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	56622d9b-4762-4c22-9ba0-b4a456b12323	\N	\N	\N	\N	\N
304	2026-07-15 10:29:33.760395+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	773db28c-f1cd-484a-94e5-30501bd9dc19	\N	\N	\N	\N	\N
305	2026-07-15 10:29:33.773563+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	6d54d86f-96a5-4b00-9dea-4be973c07e20	\N	\N	\N	\N	\N
306	2026-07-15 10:29:33.792196+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	9391066b-c66e-426d-b506-f1175d72011e	\N	\N	\N	\N	\N
307	2026-07-15 10:29:33.806734+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	f92d0e84-bf70-47c9-bc78-7b6485203866	\N	\N	\N	\N	\N
308	2026-07-15 10:29:33.820398+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	91fa88c2-7bbf-4b5d-b30e-1f2d5d33eadd	\N	\N	\N	\N	\N
309	2026-07-15 10:29:33.834199+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	9eebf1d4-6636-4e6c-8423-12504aa8128b	\N	\N	\N	\N	\N
310	2026-07-15 10:29:33.848297+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	7079f4d5-4937-44a4-8095-b3d17e575874	\N	\N	\N	\N	\N
311	2026-07-15 10:29:33.863299+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	661a73af-2d6b-4cdb-8a94-913eed8b5412	\N	\N	\N	\N	\N
312	2026-07-15 10:29:33.878085+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	c07d4438-b746-4c12-9a73-6da367c831f7	\N	\N	\N	\N	\N
313	2026-07-15 10:29:33.89149+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	6ca4a3dc-1931-4827-9703-812174db752e	\N	\N	\N	\N	\N
314	2026-07-15 10:29:33.905807+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	9d1c2689-892d-47a1-9477-4e376e8c3e9e	\N	\N	\N	\N	\N
315	2026-07-15 10:29:33.919076+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	e08d9e37-3ae9-4b03-9f5d-9a7fd5443ccb	\N	\N	\N	\N	\N
316	2026-07-15 10:29:33.933775+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	5266e116-af74-40e9-a179-fe6fe91e0f79	\N	\N	\N	\N	\N
317	2026-07-15 10:29:33.948234+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	7d45f9fe-6c27-497d-a2ec-0f1e1bdf6e80	\N	\N	\N	\N	\N
318	2026-07-15 10:29:33.96193+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	36a6da72-a05c-4f72-b08e-710fbc4bb851	\N	\N	\N	\N	\N
319	2026-07-15 10:29:33.989526+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	170ba3ef-31df-4852-8510-bafa50584fb0	\N	\N	\N	\N	\N
320	2026-07-15 10:29:34.01607+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	21958bb5-f201-414e-9d56-6bfe43cddfa0	\N	\N	\N	\N	\N
321	2026-07-15 10:29:34.033992+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	5a7b5f6b-b816-40d8-847d-699187d939e9	\N	\N	\N	\N	\N
322	2026-07-15 10:29:34.052199+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	8a54f068-90ef-4fcd-a983-74222c494f99	\N	\N	\N	\N	\N
323	2026-07-15 10:29:34.068796+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	5d9ee749-ca87-431f-858e-030280072921	\N	\N	\N	\N	\N
324	2026-07-15 10:29:50.394519+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.generate	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	{"count": 57}	\N
325	2026-07-15 10:54:12.358828+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.delete	work_log	bf5f1fef-f514-445b-8cc3-cf4e6229c73e	\N	\N	\N	\N	\N
326	2026-07-15 11:50:07.070242+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.create	work_log	efcc4556-bfe2-4f50-8db9-8f746a5ac9cf	\N	\N	\N	{"id": "efcc4556-bfe2-4f50-8db9-8f746a5ac9cf", "note": "เช็คชื่อ", "room": "", "hours": 2, "status": "", "activity": "lecture", "end_time": "12:30", "work_date": "2026-06-25", "start_time": "10:30", "assignment_id": "96f9b8d4-abc0-4372-807f-f0617bf610e7"}	\N
327	2026-07-15 11:50:33.309641+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.update	work_log	efcc4556-bfe2-4f50-8db9-8f746a5ac9cf	\N	\N	\N	{"id": "efcc4556-bfe2-4f50-8db9-8f746a5ac9cf", "note": "เช็คชื่อ", "room": "", "hours": 2, "status": "draft", "activity": "lecture", "end_time": "12:30:00", "work_date": "2026-06-25", "start_time": "10:30:00", "assignment_id": "96f9b8d4-abc0-4372-807f-f0617bf610e7"}	\N
328	2026-07-15 12:18:29.327495+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.update	work_log	efcc4556-bfe2-4f50-8db9-8f746a5ac9cf	\N	\N	\N	{"id": "efcc4556-bfe2-4f50-8db9-8f746a5ac9cf", "note": "เช็คชื่อล", "room": "", "hours": 2, "status": "draft", "activity": "lecture", "end_time": "12:30:00", "work_date": "2026-06-25", "start_time": "10:30:00", "assignment_id": "96f9b8d4-abc0-4372-807f-f0617bf610e7"}	\N
329	2026-07-15 12:18:42.524938+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.update	work_log	efcc4556-bfe2-4f50-8db9-8f746a5ac9cf	\N	\N	\N	{"id": "efcc4556-bfe2-4f50-8db9-8f746a5ac9cf", "note": "เช็คชื่อลอง", "room": "", "hours": 2, "status": "draft", "activity": "lecture", "end_time": "12:30:00", "work_date": "2026-06-25", "start_time": "10:30:00", "assignment_id": "96f9b8d4-abc0-4372-807f-f0617bf610e7"}	\N
330	2026-07-15 12:19:04.119094+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.update	work_log	efcc4556-bfe2-4f50-8db9-8f746a5ac9cf	\N	\N	\N	{"id": "efcc4556-bfe2-4f50-8db9-8f746a5ac9cf", "note": "เช็คชื่อลองใหม่", "room": "", "hours": 2, "status": "draft", "activity": "lecture", "end_time": "12:30:00", "work_date": "2026-06-25", "start_time": "10:30:00", "assignment_id": "96f9b8d4-abc0-4372-807f-f0617bf610e7"}	\N
331	2026-07-15 12:19:46.655198+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.submit	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	\N	\N
332	2026-07-15 12:36:06.01717+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	worklog.reject	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	\N	ดูข้อความหมายเหตุของวันที่ 25/06 ใหม่
333	2026-07-15 12:49:32.094525+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.update	work_log	efcc4556-bfe2-4f50-8db9-8f746a5ac9cf	\N	\N	\N	{"id": "efcc4556-bfe2-4f50-8db9-8f746a5ac9cf", "note": "เช็คชื่อ", "room": "", "hours": 2, "status": "rejected", "activity": "lecture", "end_time": "12:30:00", "work_date": "2026-06-25", "start_time": "10:30:00", "assignment_id": "96f9b8d4-abc0-4372-807f-f0617bf610e7", "reject_reason": "ดูข้อความหมายเหตุของวันที่ 25/06 ใหม่"}	\N
334	2026-07-15 12:54:17.103249+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.submit	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	\N	\N
335	2026-07-15 13:00:17.970629+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.submit	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	\N	\N
336	2026-07-15 13:00:29.688496+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	worklog.approve	assignment	96f9b8d4-abc0-4372-807f-f0617bf610e7	\N	\N	\N	\N	\N
337	2026-07-15 13:10:57.008843+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"regular": 80, "special": 0, "num_students": 80}	\N
338	2026-07-15 14:20:02.803064+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	submission_period.bulk_create	term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 5}	\N
339	2026-07-15 22:55:30.689604+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
340	2026-07-15 23:08:57.737209+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
341	2026-07-15 23:20:32.373181+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_review_schedule.add	assignment	17cf718e-223d-447e-8974-437a5ef678de	\N	\N	\N	{"note": "", "room": "", "end_time": "16:00", "start_time": "14:00", "day_of_week": 6}	\N
342	2026-07-15 23:21:59.342749+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_review_schedule.delete	ta_review_schedule	0b8267c2-5d13-4e5d-a6b7-026d778e43a1	\N	\N	\N	\N	\N
343	2026-07-15 23:43:17.497892+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
377	2026-07-16 04:14:20.219564+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
344	2026-07-16 00:40:51.82988+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	ta_review_schedule.add	assignment	17cf718e-223d-447e-8974-437a5ef678de	\N	\N	\N	{"note": "", "room": "", "end_time": "16:00", "start_time": "14:00", "day_of_week": 0}	\N
345	2026-07-16 00:41:08.372656+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.generate	assignment	17cf718e-223d-447e-8974-437a5ef678de	\N	\N	\N	{"count": 53}	\N
346	2026-07-16 00:43:31.744429+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	worklog.submit	assignment	17cf718e-223d-447e-8974-437a5ef678de	\N	\N	\N	\N	\N
347	2026-07-16 00:53:18.437027+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	submission_period.ta_signed	submission_period_status	1cb6e7c0-5d40-400a-b7f6-68b833ae46ed/a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	\N	\N
348	2026-07-16 01:34:14.519209+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
349	2026-07-16 01:34:28.166664+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	submission_period.exported	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"locked_cells": 5}	\N
350	2026-07-16 01:34:28.285848+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export.course	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	127.0.0.1	curl/8.17.0	\N	\N	\N
351	2026-07-16 01:34:28.338508+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export_batch.record	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"id": "e1736543-84bd-408c-90a3-d2494f4994f2", "ta_count": 1, "file_name": "SC363001_20260716_013428.zip", "file_path": "SC363001_20260716_013428.zip", "total_baht": 4560, "generated_at": "2026-07-15T18:34:28Z", "generated_by": "19ff049b-34d5-4e3e-be1e-1e78ed4e35ac", "teaching_course_id": "a416b931-2506-45a8-96c7-d06de9246ff7"}	\N
352	2026-07-16 01:34:51.144982+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	submission_period.finance_sent	submission_period_status	1cb6e7c0-5d40-400a-b7f6-68b833ae46ed/b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	\N	\N
353	2026-07-16 01:34:51.683456+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	submission_period.sent_back	submission_period_status	220e35af-de73-4127-9c19-123bcc7232b2/b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	{"to": "pending", "from": "exported"}	???????????
354	2026-07-16 01:44:25.186719+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
355	2026-07-16 01:47:50.268875+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	submission_period.exported	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"locked_cells": 5}	\N
356	2026-07-16 01:47:50.31731+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export.course	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
357	2026-07-16 01:47:50.332201+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export_batch.record	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"id": "b16dcf74-bba0-4ac8-973b-f7f06006bab8", "ta_count": 1, "file_name": "SC363001_20260716_014750.zip", "file_path": "SC363001_20260716_014750.zip", "total_baht": 4560, "generated_at": "2026-07-15T18:47:50Z", "generated_by": "19ff049b-34d5-4e3e-be1e-1e78ed4e35ac", "teaching_course_id": "a416b931-2506-45a8-96c7-d06de9246ff7"}	\N
358	2026-07-16 01:48:45.353577+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	submission_period.finance_sent	submission_period_status	1cb6e7c0-5d40-400a-b7f6-68b833ae46ed/b134e943-7410-44fd-883b-0b32f4a93b33	\N	\N	\N	\N	\N
359	2026-07-16 02:08:05.242263+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
360	2026-07-16 02:19:00.242105+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	submission_period.exported	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"locked_cells": 5}	\N
361	2026-07-16 02:19:00.321802+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export.course	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	127.0.0.1	curl/8.17.0	\N	\N	\N
362	2026-07-16 02:19:00.348456+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export_batch.record	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"id": "08d9c434-20ed-4151-af3e-fb9ca067e0f4", "ta_count": 1, "file_name": "SC363001_20260716_021900.zip", "file_path": "SC363001_20260716_021900.zip", "total_baht": 4560, "generated_at": "2026-07-15T19:19:00Z", "generated_by": "19ff049b-34d5-4e3e-be1e-1e78ed4e35ac", "teaching_course_id": "a416b931-2506-45a8-96c7-d06de9246ff7"}	\N
363	2026-07-16 02:39:37.688084+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
364	2026-07-16 02:40:00.524942+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	submission_period.exported	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"locked_cells": 5}	\N
365	2026-07-16 02:40:00.587178+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export.course	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	127.0.0.1	curl/8.17.0	\N	\N	\N
366	2026-07-16 02:40:00.621449+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export_batch.record	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"id": "fc62d6c6-00ec-4ee7-9db1-789755e62fed", "ta_count": 1, "file_name": "SC363001_20260716_024000.zip", "file_path": "SC363001_20260716_024000.zip", "total_baht": 4560, "generated_at": "2026-07-15T19:40:00Z", "generated_by": "19ff049b-34d5-4e3e-be1e-1e78ed4e35ac", "teaching_course_id": "a416b931-2506-45a8-96c7-d06de9246ff7"}	\N
367	2026-07-16 03:05:32.821684+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	worklog.staff_edit	work_log	21aa7ba1-9a22-48b6-b179-140a7afab56d	\N	\N	\N	{"id": "21aa7ba1-9a22-48b6-b179-140a7afab56d", "note": "ตรวจงาน", "hours": 1.5, "status": "", "activity": "review", "end_time": "15:30:00", "work_date": "2026-06-27", "start_time": "14:00:00", "assignment_id": "96f9b8d4-abc0-4372-807f-f0617bf610e7"}	\N
368	2026-07-16 03:06:48.102275+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	worklog.staff_edit	work_log	21aa7ba1-9a22-48b6-b179-140a7afab56d	\N	\N	\N	{"id": "21aa7ba1-9a22-48b6-b179-140a7afab56d", "hours": 2, "status": "", "activity": "review", "end_time": "16:00:00", "work_date": "2026-06-27", "start_time": "14:00:00", "assignment_id": "96f9b8d4-abc0-4372-807f-f0617bf610e7"}	\N
369	2026-07-16 03:06:48.425764+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	submission_period.exported	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"locked_cells": 5}	\N
370	2026-07-16 03:06:48.454284+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export.course	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	127.0.0.1	curl/8.17.0	\N	\N	\N
371	2026-07-16 03:06:48.46222+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	export_batch.record	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"id": "8db29bb5-6b20-4025-8820-71f20d43df9b", "ta_count": 1, "file_name": "SC363001_20260716_030648.zip", "file_path": "SC363001_20260716_030648.zip", "total_baht": 4560, "generated_at": "2026-07-15T20:06:48Z", "generated_by": "19ff049b-34d5-4e3e-be1e-1e78ed4e35ac", "teaching_course_id": "a416b931-2506-45a8-96c7-d06de9246ff7"}	\N
372	2026-07-16 03:56:30.172188+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
373	2026-07-16 03:56:30.883672+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	document_progress.set_stage	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"stage": 2}	\N
374	2026-07-16 03:57:15.968388+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	document_progress.set_stage	teaching_course	a416b931-2506-45a8-96c7-d06de9246ff7	\N	\N	\N	{"stage": 0}	\N
375	2026-07-16 04:04:00.997307+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	document_progress.set_stage	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"stage": 3}	\N
376	2026-07-16 04:07:06.44274+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	document_progress.set_stage	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"stage": 1}	\N
378	2026-07-16 04:15:28.458849+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	document_progress.set_stage	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"stage": 2}	\N
379	2026-07-16 04:16:09.419806+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	document_progress.set_stage	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"stage": 4}	\N
380	2026-07-16 04:57:55.549991+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	document_progress.set_stage	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"stage": 0}	\N
381	2026-07-16 05:02:17.10861+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	8c642aa5-6606-4f18-85d2-a08a852c277d	\N	\N	\N	{"id": "8c642aa5-6606-4f18-85d2-a08a852c277d", "code": "SC362004", "credits": 3, "lab_hrs": 2, "name_th": "การเขียนโปรแกรมประยุกต์บนเว็บ", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
382	2026-07-16 05:07:31.892633+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.create	teaching_course	9d5a632c-ccff-45a0-99d9-4baecee8d7ea	\N	\N	\N	{"term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [{"track": "regular", "sec_no": "1", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "15:00", "start_time": "13:00", "day_of_week": 2}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "15:00", "start_time": "13:00", "day_of_week": 5}], "num_students": 0}, {"track": "regular", "sec_no": "2", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "15:00", "start_time": "13:00", "day_of_week": 2}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "17:00", "start_time": "15:00", "day_of_week": 5}], "num_students": 0}, {"track": "special", "sec_no": "3", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "15:00", "start_time": "13:00", "day_of_week": 2}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "17:00", "start_time": "15:00", "day_of_week": 5}], "num_students": 0}], "lecturer_ids": null, "num_students": 0, "faculty_course_id": "8c642aa5-6606-4f18-85d2-a08a852c277d"}	\N
383	2026-07-16 05:17:38.451161+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
384	2026-07-16 05:20:19.257827+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.create	teaching_course	5a02b00e-4bd4-40b9-9d30-f1b5b40b9a7f	\N	\N	\N	{"term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [], "lecturer_ids": ["7a3d60b5-5091-40e2-b18a-2cf58f2b5d55"], "num_students": 0, "faculty_course_id": "ab9a68f0-dfc0-48b0-b719-5b6ef8c718af"}	\N
385	2026-07-16 05:33:02.623159+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
386	2026-07-16 05:33:03.75617+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.create	teaching_course	e112998e-1bb6-417d-b4c2-08c2b55d6882	\N	\N	\N	{"term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [{"track": "regular", "sec_no": "1", "num_students": 0}], "lecturer_ids": ["7a3d60b5-5091-40e2-b18a-2cf58f2b5d55"], "num_students": 0, "faculty_course_id": "ab9a68f0-dfc0-48b0-b719-5b6ef8c718af"}	\N
387	2026-07-16 05:33:04.060114+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.delete	teaching_course	e112998e-1bb6-417d-b4c2-08c2b55d6882	\N	\N	\N	\N	\N
388	2026-07-16 05:42:38.41593+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.create	teaching_course	ebeba786-4e4c-4361-9572-05490572aed3	\N	\N	\N	{"term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [{"track": "regular", "sec_no": "1", "num_students": 0}], "lecturer_ids": ["7a3d60b5-5091-40e2-b18a-2cf58f2b5d55"], "num_students": 0, "faculty_course_id": "ab9a68f0-dfc0-48b0-b719-5b6ef8c718af"}	\N
389	2026-07-16 05:43:52.346241+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.delete	teaching_course	ebeba786-4e4c-4361-9572-05490572aed3	\N	\N	\N	\N	\N
390	2026-07-16 05:44:42.199842+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.delete	teaching_course	9d5a632c-ccff-45a0-99d9-4baecee8d7ea	\N	\N	\N	\N	\N
391	2026-07-16 05:48:13.805778+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.create	teaching_course	1a656edf-6a1b-4de1-b441-51e02d3150de	\N	\N	\N	{"term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [{"track": "regular", "sec_no": "1", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "15:00", "start_time": "13:00", "day_of_week": 2}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "15:00", "start_time": "13:00", "day_of_week": 5}], "num_students": 0}, {"track": "regular", "sec_no": "2", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "15:00", "start_time": "13:00", "day_of_week": 2}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "17:00", "start_time": "15:00", "day_of_week": 5}], "num_students": 0}, {"track": "special", "sec_no": "3", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "15:00", "start_time": "13:00", "day_of_week": 2}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "17:00", "start_time": "15:00", "day_of_week": 5}], "num_students": 0}], "lecturer_ids": ["2abd04e1-b22d-43fa-af57-a7830de6a2ac"], "num_students": 0, "faculty_course_id": "8c642aa5-6606-4f18-85d2-a08a852c277d"}	\N
392	2026-07-16 05:50:17.564768+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.reset_password	user	2abd04e1-b22d-43fa-af57-a7830de6a2ac	\N	\N	\N	\N	\N
393	2026-07-16 05:51:24.015079+07	2abd04e1-b22d-43fa-af57-a7830de6a2ac	\N	auth.login	user	2abd04e1-b22d-43fa-af57-a7830de6a2ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
394	2026-07-16 06:03:57.023657+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
395	2026-07-16 06:10:26.594322+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
396	2026-07-16 06:15:34.166792+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
397	2026-07-16 06:28:32.299864+07	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	auth.login	user	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
398	2026-07-16 06:29:41.647599+07	2abd04e1-b22d-43fa-af57-a7830de6a2ac	\N	auth.login	user	2abd04e1-b22d-43fa-af57-a7830de6a2ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
399	2026-07-16 06:37:31.700915+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
429	2026-07-16 15:54:00.907295+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
400	2026-07-16 06:38:23.366713+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	55cb1c44-51a9-4fb3-826b-db65d293b405	\N	\N	\N	{"id": "55cb1c44-51a9-4fb3-826b-db65d293b405", "code": "CP351203", "credits": 3, "lab_hrs": 2, "name_th": "การเขียนโปรแกรมเว็บและการประยุกต์ใช้", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
401	2026-07-16 06:41:34.32033+07	2abd04e1-b22d-43fa-af57-a7830de6a2ac	\N	teaching_course.create	teaching_course	cea72167-38ab-43c8-b80c-8a4b964c90c2	\N	\N	\N	{"term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [{"track": "regular", "sec_no": "1", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "19:00", "start_time": "17:00", "day_of_week": 2}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "15:00", "start_time": "13:00", "day_of_week": 4}], "num_students": 0}, {"track": "special", "sec_no": "2", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "19:00", "start_time": "17:00", "day_of_week": 2}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "15:00", "start_time": "13:00", "day_of_week": 4}], "num_students": 0}], "lecturer_ids": null, "num_students": 0, "faculty_course_id": "55cb1c44-51a9-4fb3-826b-db65d293b405"}	\N
402	2026-07-16 06:47:32.190362+07	2abd04e1-b22d-43fa-af57-a7830de6a2ac	\N	teaching_course.create	teaching_course	ad611905-ad80-4d20-8fba-86339488b093	\N	\N	\N	{"term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [{"track": "regular", "sec_no": "1", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "15:00", "start_time": "13:00", "day_of_week": 1}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "17:00", "start_time": "15:00", "day_of_week": 1}], "num_students": 0}, {"track": "regular", "sec_no": "2", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "15:00", "start_time": "13:00", "day_of_week": 1}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "19:00", "start_time": "17:00", "day_of_week": 1}], "num_students": 0}], "lecturer_ids": null, "num_students": 0, "faculty_course_id": "02931ca7-7569-429e-907a-b9b013a07272"}	\N
403	2026-07-16 06:57:55.842572+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
404	2026-07-16 07:05:21.692014+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	worklog.staff_edit	work_log	21aa7ba1-9a22-48b6-b179-140a7afab56d	\N	\N	\N	{"id": "21aa7ba1-9a22-48b6-b179-140a7afab56d", "hours": 1, "status": "", "activity": "review", "end_time": "15:00:00", "work_date": "2026-06-27", "start_time": "14:00:00", "assignment_id": "96f9b8d4-abc0-4372-807f-f0617bf610e7"}	\N
405	2026-07-16 07:06:19.632064+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	worklog.staff_edit	work_log	21aa7ba1-9a22-48b6-b179-140a7afab56d	\N	\N	\N	{"id": "21aa7ba1-9a22-48b6-b179-140a7afab56d", "hours": 2, "status": "", "activity": "review", "end_time": "16:00:00", "work_date": "2026-06-27", "start_time": "14:00:00", "assignment_id": "96f9b8d4-abc0-4372-807f-f0617bf610e7"}	\N
406	2026-07-16 10:58:57.036768+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
407	2026-07-16 12:18:50.519432+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"regular": 50, "special": 10, "num_students": 60}	\N
408	2026-07-16 12:19:17.013579+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"regular": 0, "special": 0, "num_students": 0}	\N
409	2026-07-16 13:49:27.358783+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 9, "order_no": "20/2569"}	\N
410	2026-07-16 14:24:28.85741+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 9, "order_no": "20/2569"}	\N
411	2026-07-16 14:26:07.707769+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 9, "order_no": "20/2569"}	\N
412	2026-07-16 14:29:40.93832+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
413	2026-07-16 14:29:56.370854+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "20/2569"}	\N
414	2026-07-16 14:31:00.097916+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "20/2569"}	\N
415	2026-07-16 14:35:37.878756+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "20/2569"}	\N
416	2026-07-16 14:53:09.866987+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
417	2026-07-16 14:53:10.204485+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "20/2569"}	\N
418	2026-07-16 15:01:54.342584+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
419	2026-07-16 15:01:54.666286+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "20/2569"}	\N
420	2026-07-16 15:06:21.73173+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "6 / 2569"}	\N
421	2026-07-16 15:22:32.626344+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
422	2026-07-16 15:22:32.959277+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "20/2569"}	\N
423	2026-07-16 15:24:19.645859+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
424	2026-07-16 15:24:20.015568+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "20/2569"}	\N
425	2026-07-16 15:26:29.740004+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
426	2026-07-16 15:26:30.129272+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "20/2569"}	\N
427	2026-07-16 15:29:08.061729+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "7 / 2569"}	\N
428	2026-07-16 15:35:37.802638+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
430	2026-07-16 16:09:33.422694+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
431	2026-07-16 16:09:33.89956+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 7, "order_no": "20/2569"}	\N
432	2026-07-16 16:12:44.10247+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15	\N	\N	\N
433	2026-07-16 16:13:34.76388+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
434	2026-07-16 16:14:06.261426+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.create	user	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	\N	\N	{"email": "evenerak2547@gmail.com", "roles": ["ta"], "title": "นางสาว", "last_name": "วงศ์นอก", "first_name": "ภัทรวดี", "study_year": 4, "study_level": "undergrad"}	\N
435	2026-07-16 16:14:31.082167+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
436	2026-07-16 16:15:29.294317+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.create	faculty_course	9c8ecee4-8b8f-4670-a087-847ec7dd0e77	\N	\N	\N	{"id": "9c8ecee4-8b8f-4670-a087-847ec7dd0e77", "code": "SC362005", "credits": 3, "lab_hrs": 2, "name_th": "การวิเคราะห์และออกแบบฐานข้อมูล", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
437	2026-07-16 16:15:35.087275+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.create	user	2e25e60a-6743-4fe9-bc89-ae5c9c733730	\N	\N	\N	{"email": "nattapat.pr@kkumail.com", "roles": ["ta"], "title": "นาย", "last_name": "ประชุมวงษ์", "first_name": "ณัฐภัทร", "study_year": 4, "study_level": "undergrad"}	\N
438	2026-07-16 16:16:29.245494+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.reset_password	user	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	\N	\N	\N	\N	\N
439	2026-07-16 16:16:49.942202+07	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	\N	auth.login	user	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36 Edg/150.0.0.0	\N	\N	\N
440	2026-07-16 16:18:09.474183+07	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	\N	auth.login	user	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
441	2026-07-16 16:18:23.531378+07	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	auth.login	user	8d7211e1-8a6e-469b-b9ac-653df81f83ed	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15	\N	\N	\N
442	2026-07-16 16:22:05.81609+07	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	ta_profile.submit	ta_profile	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	\N	\N	{"round": 1}	\N
443	2026-07-16 16:22:44.656061+07	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	ta_doc.upload	ta_document	bf86c84a-6611-4d14-8e32-0555b5ec4cf3	\N	\N	\N	{"kind": "creditor_form", "round": 1, "filename": "creditor_form_ภัทรวดี_วงศ์นอก.pdf"}	\N
444	2026-07-16 16:22:50.438663+07	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	\N	teaching_course.create	teaching_course	ad4b1cff-c174-477c-827b-0a6f63e4beb8	\N	\N	\N	{"term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [{"track": "regular", "sec_no": "1", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "12:30", "start_time": "10:30", "day_of_week": 3}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "15:00", "start_time": "13:00", "day_of_week": 3}], "num_students": 0}, {"track": "regular", "sec_no": "2", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "12:30", "start_time": "10:30", "day_of_week": 3}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "15:00", "start_time": "13:00", "day_of_week": 3}], "num_students": 0}, {"track": "special", "sec_no": "3", "schedules": [{"id": "00000000-0000-0000-0000-000000000000", "kind": "lecture", "end_time": "12:30", "start_time": "10:30", "day_of_week": 3}, {"id": "00000000-0000-0000-0000-000000000000", "kind": "lab", "end_time": "15:00", "start_time": "13:00", "day_of_week": 3}], "num_students": 0}], "lecturer_ids": null, "num_students": 0, "faculty_course_id": "9c8ecee4-8b8f-4670-a087-847ec7dd0e77"}	\N
445	2026-07-16 16:23:04.156679+07	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	ta_doc.upload	ta_document	ada63b7f-0d93-4f77-9718-d00c9350ba20	\N	\N	\N	{"kind": "national_id", "round": 1, "filename": "IMG_1054.jpeg"}	\N
446	2026-07-16 16:23:44.119022+07	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	ta_doc.upload	ta_document	696f343a-0ada-4e69-8e2d-03d0ed46e86f	\N	\N	\N	{"kind": "bank_book", "round": 1, "filename": "IMG_0853.jpeg"}	\N
447	2026-07-16 16:24:21.310132+07	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	\N	ta_request.auto_decide	ta_request	27bf13f1-a400-4122-9f81-1217f525e789	\N	\N	\N	{"counts": [{"section_id": "ec11dd51-2af3-40da-afa9-8703523b837e", "graduate_count": 0, "undergrad_count": 1}, {"section_id": "80129309-def2-47ee-a51a-423fdcbd0808", "graduate_count": 0, "undergrad_count": 1}, {"section_id": "130f437e-714e-4959-923d-266135e0ae42", "graduate_count": 0, "undergrad_count": 1}], "assignments": [{"level": "undergrad", "ta_id": "8d7211e1-8a6e-469b-b9ac-653df81f83ed", "workload": {"lab_hrs": 4, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 2, "check_work_hrs": 4, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_ids": ["ec11dd51-2af3-40da-afa9-8703523b837e", "80129309-def2-47ee-a51a-423fdcbd0808", "130f437e-714e-4959-923d-266135e0ae42"]}], "reimburse_scope": "both", "teaching_course_id": "ad4b1cff-c174-477c-827b-0a6f63e4beb8"}	rejected
448	2026-07-16 16:25:18.499081+07	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	\N	ta_request.auto_decide	ta_request	5bcaa54a-781e-448f-af38-73b1e8ba94f5	\N	\N	\N	{"counts": [{"section_id": "ec11dd51-2af3-40da-afa9-8703523b837e", "graduate_count": 0, "undergrad_count": 1}, {"section_id": "80129309-def2-47ee-a51a-423fdcbd0808", "graduate_count": 0, "undergrad_count": 0}, {"section_id": "130f437e-714e-4959-923d-266135e0ae42", "graduate_count": 0, "undergrad_count": 0}], "assignments": [{"level": "undergrad", "ta_id": "8d7211e1-8a6e-469b-b9ac-653df81f83ed", "workload": {"lab_hrs": 1, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 0, "ug_other_desc": "", "attendance_hrs": 1, "check_work_hrs": 1, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_ids": ["ec11dd51-2af3-40da-afa9-8703523b837e"]}], "reimburse_scope": "both", "teaching_course_id": "ad4b1cff-c174-477c-827b-0a6f63e4beb8"}	approved
449	2026-07-16 16:26:40.896341+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_profile.approve_all	ta_profile	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	\N	\N	{"docs": ["696f343a-0ada-4e69-8e2d-03d0ed46e86f", "bf86c84a-6611-4d14-8e32-0555b5ec4cf3", "ada63b7f-0d93-4f77-9718-d00c9350ba20"], "round": 1}	\N
450	2026-07-16 16:27:20.680629+07	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	ta_review_schedule.add	assignment	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	\N	\N	\N	{"note": "", "room": "", "end_time": "16:00", "start_time": "14:00", "day_of_week": 6}	\N
454	2026-07-16 16:39:43.782298+07	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	\N	worklog.reject	assignment	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	\N	\N	\N	\N	ไล่ออก
451	2026-07-16 16:27:29.574388+07	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	worklog.generate	assignment	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	\N	\N	\N	{"count": 53}	\N
452	2026-07-16 16:33:10.316512+07	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	\N	ta_request.auto_decide	ta_request	662320f6-224e-4cea-acbe-2c34a82bdb23	\N	\N	\N	{"counts": [{"section_id": "ec11dd51-2af3-40da-afa9-8703523b837e", "graduate_count": 0, "undergrad_count": 1}, {"section_id": "80129309-def2-47ee-a51a-423fdcbd0808", "graduate_count": 0, "undergrad_count": 1}, {"section_id": "130f437e-714e-4959-923d-266135e0ae42", "graduate_count": 0, "undergrad_count": 1}], "assignments": [{"level": "undergrad", "ta_id": "2e25e60a-6743-4fe9-bc89-ae5c9c733730", "workload": {"lab_hrs": 1, "prep_hrs": 0, "grade_hrs": 0, "other_hrs": 0, "prep_desc": "", "grade_desc": "", "other_desc": "", "ug_other_hrs": 1, "ug_other_desc": "", "attendance_hrs": 1, "check_work_hrs": 1, "help_teach_hrs": 0, "help_teach_desc": ""}, "section_ids": ["ec11dd51-2af3-40da-afa9-8703523b837e", "130f437e-714e-4959-923d-266135e0ae42", "80129309-def2-47ee-a51a-423fdcbd0808"]}], "reimburse_scope": "both", "teaching_course_id": "ad4b1cff-c174-477c-827b-0a6f63e4beb8"}	rejected
453	2026-07-16 16:39:07.451403+07	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	worklog.submit	assignment	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	\N	\N	\N	\N	\N
455	2026-07-16 16:40:33.141791+07	8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	worklog.submit	assignment	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	\N	\N	\N	\N	\N
456	2026-07-16 16:58:20.205156+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (iPad; CPU OS 26_5_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/150.0.7871.51 Mobile/15E148 Safari/604.1	\N	\N	\N
457	2026-07-16 16:59:11.78822+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	ad4b1cff-c174-477c-827b-0a6f63e4beb8	\N	\N	\N	{"regular": 55, "special": 40, "num_students": 95}	\N
458	2026-07-16 16:59:26.615664+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	ad4b1cff-c174-477c-827b-0a6f63e4beb8	\N	\N	\N	{"regular": 85, "special": 10, "num_students": 95}	\N
459	2026-07-17 06:41:56.644805+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
460	2026-07-17 06:51:05.154329+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
461	2026-07-17 07:22:27.522452+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
462	2026-07-17 07:33:27.582339+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
463	2026-07-17 08:03:25.594534+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.21459.0 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
464	2026-07-17 08:47:32.647096+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
465	2026-07-17 08:48:25.634261+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.update	faculty_course	0c367ed3-f276-4e81-bbc5-6f3a2ed3082c	\N	\N	\N	{"id": "0c367ed3-f276-4e81-bbc5-6f3a2ed3082c", "code": "CP323204", "level": "graduate", "credits": 3, "lab_hrs": 2, "name_en": "Web Application Development with Java", "name_th": "การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
466	2026-07-17 08:49:12.627566+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.update	faculty_course	0c367ed3-f276-4e81-bbc5-6f3a2ed3082c	\N	\N	\N	{"id": "0c367ed3-f276-4e81-bbc5-6f3a2ed3082c", "code": "CP323204", "level": "graduate", "credits": 3, "lab_hrs": 2, "name_en": "Web Application Development with Java", "name_th": "การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
467	2026-07-17 08:50:12.237086+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 8, "order_no": "20/2569"}	\N
468	2026-07-17 08:50:41.22276+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	faculty_course.update	faculty_course	0c367ed3-f276-4e81-bbc5-6f3a2ed3082c	\N	\N	\N	{"id": "0c367ed3-f276-4e81-bbc5-6f3a2ed3082c", "code": "CP323204", "level": "undergrad", "credits": 3, "lab_hrs": 2, "name_th": "การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา", "self_hrs": 5, "is_active": true, "lecture_hrs": 2}	\N
469	2026-07-17 09:00:58.262936+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
470	2026-07-17 09:00:58.598189+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 8, "order_no": "6/2569"}	\N
471	2026-07-17 09:03:24.350464+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
472	2026-07-17 09:03:24.681352+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 8, "order_no": "6/2569"}	\N
473	2026-07-17 09:34:06.545195+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	curl/8.17.0	\N	\N	\N
474	2026-07-17 09:34:58.124608+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 8, "order_no": "6/2569"}	\N
475	2026-07-17 10:08:08.152706+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36	\N	\N	\N
476	2026-07-17 10:17:52.510177+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.reset_password	user	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	\N	\N	\N	\N
477	2026-07-17 10:18:15.068261+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.deactivate	user	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	\N	\N	\N	\N
478	2026-07-17 10:18:19.667843+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.activate	user	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	\N	\N	\N	\N
479	2026-07-17 10:23:52.536089+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
480	2026-07-17 10:25:41.49369+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	appointment_order.build	academic_term	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	\N	{"count": 8, "order_no": "7/2569"}	\N
481	2026-07-17 10:33:39.042576+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5.2 Safari/605.1.15	\N	\N	\N
482	2026-07-17 10:47:37.287876+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_docs.reject_batch	ta_profile	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	\N	\N	{"items": [{"doc_id": "dc46188f-67fa-48b3-9be0-906b74a21eed", "reason": "ไม่ชัด"}], "round": 1, "batch_id": "b4272d60-f4c2-4123-b735-c9a8785b1505"}	\N
483	2026-07-17 10:47:45.044501+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	ta_profile.approve_all	ta_profile	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	\N	\N	{"docs": ["fbfb19fc-ff2f-4fda-94f9-305a5f8c5b8d", "f85dccda-af40-4e1b-8b03-1d30a6084f10", "dc46188f-67fa-48b3-9be0-906b74a21eed"], "round": 1}	\N
484	2026-07-17 10:52:37.240258+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
485	2026-07-17 11:17:55.632055+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
486	2026-07-17 11:26:37.136569+07	2abd04e1-b22d-43fa-af57-a7830de6a2ac	\N	auth.login	user	2abd04e1-b22d-43fa-af57-a7830de6a2ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
487	2026-07-17 11:27:10.545608+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5.2 Safari/605.1.15	\N	\N	\N
488	2026-07-17 11:27:26.638179+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	1a656edf-6a1b-4de1-b441-51e02d3150de	\N	\N	\N	{"regular": 80, "special": 0, "num_students": 80}	\N
489	2026-07-17 12:11:29.329046+07	2abd04e1-b22d-43fa-af57-a7830de6a2ac	\N	auth.login	user	2abd04e1-b22d-43fa-af57-a7830de6a2ac	127.0.0.1	Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36	\N	\N	\N
490	2026-07-21 18:11:39.534668+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.22209.3 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
491	2026-07-21 18:28:57.718555+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.22209.3 Chrome/148.0.7778.271 Electron/42.5.1 Safari/537.36 MSIX	\N	\N	\N
492	2026-07-23 12:31:11.269434+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.24012.1 Chrome/148.0.7778.280 Electron/42.7.0 Safari/537.36 MSIX	\N	\N	\N
493	2026-07-23 13:35:59.217844+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
494	2026-07-23 13:44:53.926786+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.24012.1 Chrome/148.0.7778.280 Electron/42.7.0 Safari/537.36 MSIX	\N	\N	\N
495	2026-07-23 13:49:04.565064+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	holiday.create	holiday	f099fe0c-9840-4c0f-bbc5-dfd28a7394e9	\N	\N	\N	\N	\N
496	2026-07-23 13:51:12.347356+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	holiday.delete	holiday	f099fe0c-9840-4c0f-bbc5-dfd28a7394e9	\N	\N	\N	\N	\N
497	2026-07-23 14:19:53.647576+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	signature_checklist.toggle	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"role": "ta", "signed": true}	\N
498	2026-07-23 14:20:46.827367+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	signature_checklist.toggle	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"role": "ta", "signed": false}	\N
499	2026-07-23 15:43:07.587731+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.24012.1 Chrome/148.0.7778.280 Electron/42.7.0 Safari/537.36 MSIX	\N	\N	\N
500	2026-07-23 16:02:57.561856+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.24012.1 Chrome/148.0.7778.280 Electron/42.7.0 Safari/537.36 MSIX	\N	\N	\N
501	2026-07-23 16:33:33.64451+07	\N	\N	ta_doc.expire	ta_document	bf86c84a-6611-4d14-8e32-0555b5ec4cf3	\N	\N	{"storage_key": "ta_docs/2026/07/16/b7c1d556-b8c3-44aa-8291-d481719653cf.pdf.enc"}	\N	\N
502	2026-07-23 16:33:33.660359+07	\N	\N	ta_doc.expire	ta_document	ada63b7f-0d93-4f77-9718-d00c9350ba20	\N	\N	{"storage_key": "ta_docs/2026/07/16/5a75ca88-71d9-4f7e-9e29-f4813dbcaf93.jpeg.enc"}	\N	\N
503	2026-07-23 16:33:33.666291+07	\N	\N	ta_doc.expire	ta_document	696f343a-0ada-4e69-8e2d-03d0ed46e86f	\N	\N	{"storage_key": "ta_docs/2026/07/16/ed8917bf-47df-4bef-99ca-50a572d56e6b.jpeg.enc"}	\N	\N
504	2026-07-23 17:29:07.265307+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	schedule.import	term	682542ce-52c5-4950-a431-60e23454b71e	\N	\N	\N	{"row_count": 127, "error_count": 65, "created_count": 127, "skipped_count": 0}	\N
505	2026-07-23 18:28:40.181716+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.create	teaching_course	c77236b8-1124-45c5-826c-2f384814ae4f	\N	\N	\N	{"code": "CP999999", "level": "undergrad", "credits": 3, "lab_hrs": 0, "name_en": "WBA TEST COURSE", "name_th": "WBA TEST COURSE", "term_id": "2a01f439-a013-4f5f-a819-5ef591497243", "sections": [{"track": "regular", "sec_no": "1", "num_students": 40}], "self_hrs": 6, "lecture_hrs": 3, "lecturer_ids": ["9cca9058-2ec5-4d6c-aa6e-82c30e5a699d"], "num_students": 0}	\N
506	2026-07-23 18:32:41.869478+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.delete	teaching_course	c77236b8-1124-45c5-826c-2f384814ae4f	\N	\N	\N	\N	\N
507	2026-07-23 18:50:41.355863+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"regular": 30, "special": 5, "num_students": 35}	\N
508	2026-07-23 18:51:08.760716+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	\N	\N	{"regular": 0, "special": 0, "num_students": 0}	\N
509	2026-07-23 19:15:03.66602+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
510	2026-07-23 19:42:09.779902+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	user.reset_password	user	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	\N	\N	\N	\N
511	2026-07-23 19:42:15.437416+07	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	auth.login	user	67959d3a-87dc-476f-ab3e-0ce6c054a444	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36	\N	\N	\N
512	2026-07-23 19:43:15.954472+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	teaching_course.num_students	teaching_course	1c0b6bb3-87ec-43ff-9b63-ff0a491ad4d7	\N	\N	\N	{"regular": 100, "special": 0, "num_students": 100}	\N
513	2026-07-23 19:43:42.821552+07	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	ta_review_schedule.add	assignment	f2238f47-c954-48d3-b7d6-6b88c60f33d5	\N	\N	\N	{"note": "", "room": "", "end_time": "16:00", "start_time": "14:00", "day_of_week": 6}	\N
514	2026-07-23 19:43:51.428815+07	67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	worklog.generate	assignment	f2238f47-c954-48d3-b7d6-6b88c60f33d5	\N	\N	\N	{"count": 51}	\N
515	2026-07-24 07:44:13.527089+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	auth.login	user	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.24012.1 Chrome/148.0.7778.280 Electron/42.7.0 Safari/537.36 MSIX	\N	\N	\N
516	2026-07-24 07:56:01.582624+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	auth.login	user	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.24012.1 Chrome/148.0.7778.280 Electron/42.7.0 Safari/537.36 MSIX	\N	\N	\N
517	2026-07-24 09:20:55.170166+07	b134e943-7410-44fd-883b-0b32f4a93b33	\N	auth.login	user	b134e943-7410-44fd-883b-0b32f4a93b33	127.0.0.1	Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Claude/1.24012.1 Chrome/148.0.7778.280 Electron/42.7.0 Safari/537.36 MSIX	\N	\N	\N
\.


--
-- Data for Name: budget_caps; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.budget_caps (id, effective_from, per_course_max, note, created_at) FROM stdin;
78b765f3-2e22-41be-a393-e5a4c2a26fb6	2026-07-08	20000.00	seed default	2026-07-08 01:55:56.527351+07
\.


--
-- Data for Name: document_progress; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.document_progress (term_id, stage, ta_signed_at, lecturer_signed_at, certifier_signed_at, sent_finance_at, sent_treasury_at, note, updated_by, updated_by_name, updated_at) FROM stdin;
2a01f439-a013-4f5f-a819-5ef591497243	0	\N	\N	\N	\N	\N	TA+??????????????	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	Admin COCO	2026-07-16 04:57:55.542727+07
\.


--
-- Data for Name: exam_schedules; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.exam_schedules (id, section_id, kind, exam_date, start_time, end_time, room) FROM stdin;
\.


--
-- Data for Name: export_batches; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.export_batches (id, teaching_course_id, submission_period_id, file_path, file_name, ta_count, total_baht, generated_at, generated_by) FROM stdin;
e61c6057-a53f-485e-b046-a1014da40990	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	CP323204_20260713_071636.zip	CP323204_20260713_071636.zip	0	0.00	2026-07-13 07:16:36+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac
\.


--
-- Data for Name: exports; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.exports (id, term_id, scope, scope_id, filename, storage_key, size_bytes, created_by, created_at) FROM stdin;
\.


--
-- Data for Name: holiday_remind_log; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.holiday_remind_log (id, ta_id, teaching_course_id, original_date, note, sent_at) FROM stdin;
\.


--
-- Data for Name: lecture_review_dates; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.lecture_review_dates (id, section_id, review_date, start_time, end_time, hours, note) FROM stdin;
\.


--
-- Data for Name: makeup_schedules; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.makeup_schedules (id, section_id, original_date, makeup_date, start_time, end_time, note) FROM stdin;
\.


--
-- Data for Name: notifications; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.notifications (id, user_id, channel, title, body, link, read_at, sent_at, created_at) FROM stdin;
e91c39b4-4266-472a-8b2b-5500d9f07ffc	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	email	คำขอ TA ได้รับการอนุมัติ	คำขอผู้ช่วยสอนวิชา CP323204 การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา ได้รับการอนุมัติแล้ว	/lecturer	\N	2026-07-09 22:28:16.699478+07	2026-07-09 22:28:16.699478+07
ad8366ba-4ba0-4116-8c6e-dd7948ca95d0	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	in_app	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP323204 การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา	/ta	\N	\N	2026-07-09 22:28:16.702572+07
184f0f1d-d6fc-405d-88de-84cb1073beda	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	email	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP323204 การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา	/ta	\N	2026-07-09 22:28:16.707217+07	2026-07-09 22:28:16.707217+07
d9688b7c-4264-4bc3-991d-5c3edcac8a1b	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP323204 การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา	/ta	\N	\N	2026-07-09 22:28:16.71072+07
de2d6f9a-e82c-48c5-ad0d-82bdd521e158	b134e943-7410-44fd-883b-0b32f4a93b33	email	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP323204 การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา	/ta	\N	2026-07-09 22:28:16.715561+07	2026-07-09 22:28:16.715561+07
ca9a0121-f8dc-4d83-814a-22d65fe86e63	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	in_app	คำขอ TA ได้รับการอนุมัติ	คำขอผู้ช่วยสอนวิชา CP323204 การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา ได้รับการอนุมัติแล้ว	/lecturer	2026-07-10 02:03:21.709106+07	\N	2026-07-09 22:28:16.690528+07
561d106b-4ec8-4a56-acee-eff97e2af1c5	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	in_app	เจ้าหน้าที่แก้ไขบันทึกเวลา	เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ 2026-06-22 เวลา 09:00:00–11:00:00	/ta/worklog	\N	\N	2026-07-13 07:15:39.480957+07
c5e329a9-f5e8-4bf8-a405-258f2df121ad	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	email	เจ้าหน้าที่แก้ไขบันทึกเวลา	เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ 2026-06-22 เวลา 09:00:00–11:00:00	/ta/worklog	\N	2026-07-13 07:15:39.487237+07	2026-07-13 07:15:39.487237+07
6ea4de5e-eb05-491e-8995-d4993eebff91	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	email	คำขอ TA ถูกปฏิเสธ	คำขอผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์ ถูกระบบปฏิเสธอัตโนมัติ: ธนเดช วาตรีบุญเรือง ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่ • จุฑามาศ ชะรานันท์ ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่ • เวลาสอนของ section นี้ทับซ้อนกับตารางเรียนของ สุพพิธา…	/lecturer	\N	2026-07-14 05:32:14.523953+07	2026-07-14 05:32:14.523953+07
9812b7cf-6226-4240-8595-9656eb4dce29	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	in_app	คำขอ TA ถูกปฏิเสธ	คำขอผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์ ถูกระบบปฏิเสธอัตโนมัติ: ธนเดช วาตรีบุญเรือง ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่ • จุฑามาศ ชะรานันท์ ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่ • เวลาสอนของ section นี้ทับซ้อนกับตารางเรียนของ สุพพิธา…	/lecturer	2026-07-14 05:51:15.591794+07	\N	2026-07-14 05:32:14.517457+07
5d2caac0-2a0f-4ac4-ae20-135112e962df	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	in_app	คำขอ TA ได้รับการอนุมัติ	คำขอผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์ ได้รับการอนุมัติจากระบบอัตโนมัติแล้ว	/lecturer	\N	\N	2026-07-14 05:54:14.249379+07
b51bba49-8ad2-4dff-a229-a1ab0b8f67cd	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	email	คำขอ TA ได้รับการอนุมัติ	คำขอผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์ ได้รับการอนุมัติจากระบบอัตโนมัติแล้ว	/lecturer	\N	2026-07-14 05:54:14.254762+07	2026-07-14 05:54:14.254762+07
2b625cda-951a-4c26-8b18-3b99cf66bedd	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	in_app	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์	/ta	\N	\N	2026-07-14 05:54:14.259639+07
b077630a-10d9-4c3b-be28-3976ff183aa5	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	email	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์	/ta	\N	2026-07-14 05:54:14.264756+07	2026-07-14 05:54:14.264756+07
79ea6f55-f9f2-44fb-b158-66e93fa59e34	67959d3a-87dc-476f-ab3e-0ce6c054a444	in_app	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์	/ta	\N	\N	2026-07-14 05:54:14.267173+07
6e850f9a-e341-4bb5-8365-dac64ddd4309	67959d3a-87dc-476f-ab3e-0ce6c054a444	email	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์	/ta	\N	2026-07-14 05:54:14.273052+07	2026-07-14 05:54:14.273052+07
f101d547-41ec-4b94-be0a-c5ee23ccff78	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	in_app	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์	/ta	\N	\N	2026-07-14 05:54:14.276034+07
cd23d8ac-8c72-418b-8679-700bd97bb87b	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	email	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์	/ta	\N	2026-07-14 05:54:14.279837+07	2026-07-14 05:54:14.279837+07
c02a4b42-7af6-4f5a-a612-8161a5cfc30d	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์	/ta	\N	\N	2026-07-14 05:54:14.282501+07
0ee8d0a3-942d-4e6d-97ef-582de0481af4	b134e943-7410-44fd-883b-0b32f4a93b33	email	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา CP421025 การวิเคราะห์และออกแบบซอฟต์แวร์	/ta	\N	2026-07-14 05:54:14.285985+07	2026-07-14 05:54:14.285985+07
6f3d656e-c8fb-4d9f-b11f-93e2d621ea36	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	in_app	คำขอ TA ได้รับการอนุมัติ	คำขอผู้ช่วยสอนวิชา SC363001 การวิเคราะห์และออกแบบระบบ ได้รับการอนุมัติจากระบบอัตโนมัติแล้ว	/lecturer	\N	\N	2026-07-14 10:11:09.194415+07
e6884609-6a92-4558-b000-e15b6e6de364	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	email	คำขอ TA ได้รับการอนุมัติ	คำขอผู้ช่วยสอนวิชา SC363001 การวิเคราะห์และออกแบบระบบ ได้รับการอนุมัติจากระบบอัตโนมัติแล้ว	/lecturer	\N	2026-07-14 10:11:09.199417+07	2026-07-14 10:11:09.199417+07
a9517c70-cdb4-4834-825e-f52159c971e6	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา SC363001 การวิเคราะห์และออกแบบระบบ	/ta	\N	\N	2026-07-14 10:11:09.204191+07
9d117b6b-7be6-4134-b425-8b361c95d91f	b134e943-7410-44fd-883b-0b32f4a93b33	email	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา SC363001 การวิเคราะห์และออกแบบระบบ	/ta	\N	2026-07-14 10:11:09.208232+07	2026-07-14 10:11:09.208232+07
eaec916d-08ac-4c5d-aba3-2fa85337ea35	b134e943-7410-44fd-883b-0b32f4a93b33	email	บันทึกเวลาถูกปฏิเสธ	บันทึกเวลาของคุณถูกส่งกลับให้แก้ไข: ดูข้อความหมายเหตุของวันที่ 25/06 ใหม่	/ta/worklog	\N	2026-07-15 12:36:06.033065+07	2026-07-15 12:36:06.033065+07
7be51e4e-458d-4887-bc1a-d7ee9e11e224	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	บันทึกเวลาถูกปฏิเสธ	บันทึกเวลาของคุณถูกส่งกลับให้แก้ไข: ดูข้อความหมายเหตุของวันที่ 25/06 ใหม่	/ta/worklog	2026-07-15 12:36:54.855347+07	\N	2026-07-15 12:36:06.0234+07
2047c345-0c2b-4d42-9d4e-b47b9e61349a	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	อนุมัติบันทึกเวลา	บันทึกเวลาปฏิบัติงานของคุณได้รับการอนุมัติแล้ว	/ta/worklog	\N	\N	2026-07-15 13:00:29.693473+07
87b89d8a-8258-4653-a6fc-057a3a13dabc	b134e943-7410-44fd-883b-0b32f4a93b33	email	อนุมัติบันทึกเวลา	บันทึกเวลาปฏิบัติงานของคุณได้รับการอนุมัติแล้ว	/ta/worklog	\N	2026-07-15 13:00:29.699771+07	2026-07-15 13:00:29.699771+07
902a85c0-b047-4b62-b2d6-6b49ed04d166	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	in_app	มีบันทึกเวลารอการอนุมัติ	TA ส่งบันทึกเวลาปฏิบัติงานรอการอนุมัติจากคุณ	/lecturer/approvals	\N	\N	2026-07-16 00:43:31.753271+07
ce5fcd88-dc07-4012-86e2-7f2e9307b22c	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	email	มีบันทึกเวลารอการอนุมัติ	TA ส่งบันทึกเวลาปฏิบัติงานรอการอนุมัติจากคุณ	/lecturer/approvals	\N	2026-07-16 00:43:31.76818+07	2026-07-16 00:43:31.76818+07
cc79471e-7520-40f2-a4a5-baab9c0c4be7	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 01:34:28.174682+07
72d01c4a-444e-4357-b39f-ca37241f588a	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 01:34:28.186689+07	2026-07-16 01:34:28.186689+07
3621118a-691e-450f-81b4-c24d9f980efd	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 01:34:28.207002+07
486e5aa8-a1fd-4664-810e-5ca900f8b38f	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 01:34:28.21969+07	2026-07-16 01:34:28.21969+07
daf88f06-2784-47e2-9cf0-a56339bdac70	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 01:34:28.227713+07
46c17160-44f3-4bf5-a23d-8645fa17a94d	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 01:34:28.240091+07	2026-07-16 01:34:28.240091+07
6b1602dd-bd51-4547-a6bd-1f4387bac9d3	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 01:34:28.253407+07
999e57ef-0ce6-4a9b-a1ae-e229864d0111	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 01:34:28.261643+07	2026-07-16 01:34:28.261643+07
3b9c46d8-56c0-4a52-9ad5-1355ce8bc283	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 01:34:28.267514+07
06d65764-e48c-4332-96cf-8e7c03652f61	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 01:34:28.278197+07	2026-07-16 01:34:28.278197+07
14f42ea5-e2c4-4bdc-b311-77e4a3f46f6e	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	ส่งเบิกจ่ายไปยังการเงินแล้ว	บันทึกเวลาประจำเดือนของคุณถูกส่งไปยังการเงินเรียบร้อยแล้ว	/ta/reminders	\N	\N	2026-07-16 01:34:51.152286+07
11c8be0d-0b8b-4d4b-a2d4-e0905af8f036	b134e943-7410-44fd-883b-0b32f4a93b33	email	ส่งเบิกจ่ายไปยังการเงินแล้ว	บันทึกเวลาประจำเดือนของคุณถูกส่งไปยังการเงินเรียบร้อยแล้ว	/ta/reminders	\N	2026-07-16 01:34:51.169223+07	2026-07-16 01:34:51.169223+07
72d66c9a-a8f1-4b96-b656-46d91d318d63	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	บันทึกเวลาประจำเดือนถูกตีกลับ	บันทึกเวลาประจำเดือนของคุณถูกตีกลับให้แก้ไข: ???????????	/ta/reminders	\N	\N	2026-07-16 01:34:51.689033+07
a7630642-8a25-4a64-801c-986eed7560c5	b134e943-7410-44fd-883b-0b32f4a93b33	email	บันทึกเวลาประจำเดือนถูกตีกลับ	บันทึกเวลาประจำเดือนของคุณถูกตีกลับให้แก้ไข: ???????????	/ta/reminders	\N	2026-07-16 01:34:51.69438+07	2026-07-16 01:34:51.69438+07
295be016-d03b-4241-9071-36771e55d5fa	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	in_app	บันทึกเวลาประจำเดือนถูกตีกลับ	เจ้าหน้าที่ตีกลับบันทึกเวลาประจำเดือนของ TA ในรายวิชาของคุณ: ???????????	/lecturer/approvals	\N	\N	2026-07-16 01:34:51.700996+07
b6cbb9b6-9533-4f21-8573-e994ce1939c6	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	email	บันทึกเวลาประจำเดือนถูกตีกลับ	เจ้าหน้าที่ตีกลับบันทึกเวลาประจำเดือนของ TA ในรายวิชาของคุณ: ???????????	/lecturer/approvals	\N	2026-07-16 01:34:51.706248+07	2026-07-16 01:34:51.706248+07
29921cb8-1b7c-4670-832b-28b2ca7c6634	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 01:47:50.274576+07
2c47d953-4130-4e0b-a366-6a2982435b79	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 01:47:50.280132+07	2026-07-16 01:47:50.280132+07
a401aa25-d158-4885-932c-96649691592f	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 01:47:50.283151+07
d822c86c-4506-49bc-97e7-235994fc531a	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 01:47:50.286512+07	2026-07-16 01:47:50.286512+07
efb0071c-2a7a-4749-a4e5-261b8edaa152	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 01:47:50.289225+07
2b4ce9c3-bac2-478c-964c-75532aa11709	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 01:47:50.292483+07	2026-07-16 01:47:50.292483+07
6b151a96-9cf5-4063-ba02-4ebf532f5f35	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 01:47:50.294916+07
7c90958e-e3e5-4780-9133-d8773dfb840e	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 01:47:50.299774+07	2026-07-16 01:47:50.299774+07
ae67370c-a802-40f9-8daf-5fd154f6d34e	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 01:47:50.310062+07
8ced4060-c038-40bc-a54f-827156a1a775	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 01:47:50.314402+07	2026-07-16 01:47:50.314402+07
fd21ff9d-173e-4753-a1e8-03aa77f28a53	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	ส่งเบิกจ่ายไปยังการเงินแล้ว	บันทึกเวลาประจำเดือนของคุณถูกส่งไปยังการเงินเรียบร้อยแล้ว	/ta/reminders	\N	\N	2026-07-16 01:48:45.379167+07
8bc986bd-eb29-49de-862e-bca498729140	b134e943-7410-44fd-883b-0b32f4a93b33	email	ส่งเบิกจ่ายไปยังการเงินแล้ว	บันทึกเวลาประจำเดือนของคุณถูกส่งไปยังการเงินเรียบร้อยแล้ว	/ta/reminders	\N	2026-07-16 01:48:45.392804+07	2026-07-16 01:48:45.392804+07
840b4f49-eb55-42f3-8dea-e391e5f60cac	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 02:19:00.259145+07
80810372-aa88-4aa7-be76-e2947cf42865	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 02:19:00.269217+07	2026-07-16 02:19:00.269217+07
82e2246e-e5cd-4628-a471-da57529b51b8	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 02:19:00.274343+07
06b8efd2-91fc-44f5-9ccc-8f2ff8bfd8b4	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 02:19:00.285318+07	2026-07-16 02:19:00.285318+07
803b2072-0d37-4506-8ecd-32946e1f5fa3	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 02:19:00.291401+07
7e382920-85a4-4f17-980b-c67a58e49e88	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 02:19:00.298426+07	2026-07-16 02:19:00.298426+07
b19834ab-7a2a-4ecf-873e-085cd4a06041	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 02:19:00.302958+07
c9a4dea7-c3e2-4d40-a196-e4d317afb845	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 02:19:00.308224+07	2026-07-16 02:19:00.308224+07
b21eedd6-d4ae-4063-909a-cd2891a78776	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 02:19:00.314253+07
07a79be7-1cc3-4743-8d13-7e4310d4879c	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 02:19:00.317933+07	2026-07-16 02:19:00.317933+07
86099f70-ff71-429e-98fe-8b2f821d497a	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 02:40:00.530754+07
96de0edb-81c2-4059-b9f2-c700220edd88	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 02:40:00.539604+07	2026-07-16 02:40:00.539604+07
13ad7bb0-6591-4528-8e77-7484e9433321	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 02:40:00.543085+07
51b8a527-4a04-429e-afb1-b02e0cbc2b0d	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 02:40:00.549056+07	2026-07-16 02:40:00.549056+07
aeb8d7e1-ed35-4445-b0f9-24a8d5a3f60b	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 02:40:00.555779+07
a07c060f-2dd9-4e1e-ae03-388f5221d9a4	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 02:40:00.560892+07	2026-07-16 02:40:00.560892+07
0e9c5fe7-1156-4299-8dd0-28c4eb111757	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 02:40:00.564472+07
2176f900-b3db-4658-abc1-6ad32e5cdf4f	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 02:40:00.568752+07	2026-07-16 02:40:00.568752+07
36048083-363a-4e58-89cc-f1c3406324e0	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 02:40:00.573754+07
b6b06fa9-5715-4a5c-816a-fa4c2d505f4a	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 02:40:00.582289+07	2026-07-16 02:40:00.582289+07
9215abd3-a14f-4400-a986-318d8b250baf	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่แก้ไขบันทึกเวลา	เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ 2026-06-27 เวลา 14:00:00–15:30:00	/ta/worklog	\N	\N	2026-07-16 03:05:32.827439+07
a0f3a527-7049-4696-b883-d09c4533f88a	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่แก้ไขบันทึกเวลา	เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ 2026-06-27 เวลา 14:00:00–15:30:00	/ta/worklog	\N	2026-07-16 03:05:32.833021+07	2026-07-16 03:05:32.833021+07
76f2c954-13bc-4f6c-a13b-8fa820c20306	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่แก้ไขบันทึกเวลา	เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ 2026-06-27 เวลา 14:00:00–16:00:00	/ta/worklog	\N	\N	2026-07-16 03:06:48.105853+07
e0a772ca-c373-4019-8019-c94ad5831375	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่แก้ไขบันทึกเวลา	เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ 2026-06-27 เวลา 14:00:00–16:00:00	/ta/worklog	\N	2026-07-16 03:06:48.108921+07	2026-07-16 03:06:48.108921+07
ef38b945-1905-4806-9284-27c9195b05a1	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 03:06:48.428295+07
36d027b1-24ed-47ad-86ad-3643b43d948d	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 03:06:48.431373+07	2026-07-16 03:06:48.431373+07
dce7702a-5731-4214-be1b-05bdc578eb58	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 03:06:48.433799+07
4ffcb777-8941-4097-988a-c5f04bd1f301	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 03:06:48.436697+07	2026-07-16 03:06:48.436697+07
9ee743b6-8d40-4399-980b-d36313c69956	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 03:06:48.439012+07
4d541180-e7e8-4dfd-a897-951de1e7cbc3	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 03:06:48.442062+07	2026-07-16 03:06:48.442062+07
0f6b634d-a7e5-4f19-808a-849827591580	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 03:06:48.444435+07
a44bdd1a-0596-4612-a975-98ce7830988f	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 03:06:48.447181+07	2026-07-16 03:06:48.447181+07
7d017376-8743-45b2-9c3c-14f49aac62f6	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	\N	2026-07-16 03:06:48.449576+07
afdc1a1e-9e24-41b5-9419-8f4244800a7a	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่ส่งออกเอกสารเบิกจ่ายแล้ว	เจ้าหน้าที่ตรวจสอบและส่งออกไฟล์เบิกจ่ายของคุณแล้ว — บันทึกเวลาเดือนดังกล่าวถูกล็อก	/ta/reminders	\N	2026-07-16 03:06:48.452161+07	2026-07-16 03:06:48.452161+07
0da8395c-8be5-41c2-8ec4-4a22c7768685	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่แก้ไขบันทึกเวลา	เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ 2026-06-27 เวลา 14:00:00–15:00:00	/ta/worklog	\N	\N	2026-07-16 07:05:21.698824+07
8cf7d100-7592-4c1c-ac84-08baa5751c56	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่แก้ไขบันทึกเวลา	เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ 2026-06-27 เวลา 14:00:00–15:00:00	/ta/worklog	\N	2026-07-16 07:05:21.704012+07	2026-07-16 07:05:21.704012+07
14ae1890-3e3b-4a92-8c5f-d328d0e4874b	b134e943-7410-44fd-883b-0b32f4a93b33	in_app	เจ้าหน้าที่แก้ไขบันทึกเวลา	เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ 2026-06-27 เวลา 14:00:00–16:00:00	/ta/worklog	\N	\N	2026-07-16 07:06:19.639926+07
3b28249b-23bf-4b88-9dcf-61329460167e	b134e943-7410-44fd-883b-0b32f4a93b33	email	เจ้าหน้าที่แก้ไขบันทึกเวลา	เจ้าหน้าที่ปรับข้อมูลบันทึกเวลาวันที่ 2026-06-27 เวลา 14:00:00–16:00:00	/ta/worklog	\N	2026-07-16 07:06:19.64696+07	2026-07-16 07:06:19.64696+07
e6bae03a-4993-4367-b5f2-ce723cd94087	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	in_app	คำขอ TA ถูกปฏิเสธ	คำขอผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล ถูกระบบปฏิเสธอัตโนมัติ: ภัทรวดี วงศ์นอก ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้	/lecturer	\N	\N	2026-07-16 16:24:21.317515+07
9a4d8676-ecdd-4e5c-ba94-838957d827bc	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	email	คำขอ TA ถูกปฏิเสธ	คำขอผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล ถูกระบบปฏิเสธอัตโนมัติ: ภัทรวดี วงศ์นอก ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้	/lecturer	\N	2026-07-16 16:24:21.323011+07	2026-07-16 16:24:21.323011+07
7a813991-361f-4e8d-9f53-dcd5a4dffff9	8d7211e1-8a6e-469b-b9ac-653df81f83ed	in_app	คำขอแต่งตั้งผู้ช่วยสอนไม่ผ่านการอนุมัติ	คำขอผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล ที่ระบุชื่อคุณไม่ผ่านการอนุมัติ: ภัทรวดี วงศ์นอก ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้	/ta	\N	\N	2026-07-16 16:24:21.328638+07
3d1308b4-a5b0-45b6-8d88-fce88a78bae0	8d7211e1-8a6e-469b-b9ac-653df81f83ed	email	คำขอแต่งตั้งผู้ช่วยสอนไม่ผ่านการอนุมัติ	คำขอผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล ที่ระบุชื่อคุณไม่ผ่านการอนุมัติ: ภัทรวดี วงศ์นอก ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้	/ta	\N	2026-07-16 16:24:21.33365+07	2026-07-16 16:24:21.33365+07
ed906426-61fc-4410-b88b-bea534eb60f2	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	in_app	คำขอ TA ได้รับการอนุมัติ	คำขอผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล ได้รับการอนุมัติจากระบบอัตโนมัติแล้ว	/lecturer	\N	\N	2026-07-16 16:25:18.506906+07
d0711be6-993b-4c9f-b46b-dd345eceb645	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	email	คำขอ TA ได้รับการอนุมัติ	คำขอผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล ได้รับการอนุมัติจากระบบอัตโนมัติแล้ว	/lecturer	\N	2026-07-16 16:25:18.512892+07	2026-07-16 16:25:18.512892+07
eaef1813-ed3d-44df-a57d-de40147c2fcf	8d7211e1-8a6e-469b-b9ac-653df81f83ed	in_app	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล	/ta	\N	\N	2026-07-16 16:25:18.518415+07
f3fc74e0-35ab-448e-bb8d-5cdabf8c5315	8d7211e1-8a6e-469b-b9ac-653df81f83ed	email	คุณได้รับมอบหมายเป็นผู้ช่วยสอน	คุณได้รับอนุมัติเป็นผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล	/ta	\N	2026-07-16 16:25:18.524114+07	2026-07-16 16:25:18.524114+07
8ccd7822-b4f8-4098-b02f-3caf0f01c0d7	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	in_app	คำขอ TA ถูกปฏิเสธ	คำขอผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล ถูกระบบปฏิเสธอัตโนมัติ: ณัฐภัทร ประชุมวงษ์ ยังไม่ได้บันทึกตารางเรียนของภาคการศึกษานี้ • ณัฐภัทร ประชุมวงษ์ ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้	/lecturer	\N	\N	2026-07-16 16:33:10.322212+07
a2c4db9e-f2ac-436a-a71d-32433c84c328	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	in_app	มีบันทึกเวลารอการอนุมัติ	TA ส่งบันทึกเวลาปฏิบัติงานรอการอนุมัติจากคุณ	/lecturer/approvals	\N	\N	2026-07-16 16:39:07.456859+07
f3c085c0-b4c7-4f91-9335-d4e6db5d4508	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	email	มีบันทึกเวลารอการอนุมัติ	TA ส่งบันทึกเวลาปฏิบัติงานรอการอนุมัติจากคุณ	/lecturer/approvals	\N	2026-07-16 16:39:07.46075+07	2026-07-16 16:39:07.46075+07
7eadc503-4566-4775-9817-107f5458c250	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	email	คำขอ TA ถูกปฏิเสธ	คำขอผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล ถูกระบบปฏิเสธอัตโนมัติ: ณัฐภัทร ประชุมวงษ์ ยังไม่ได้บันทึกตารางเรียนของภาคการศึกษานี้ • ณัฐภัทร ประชุมวงษ์ ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้	/lecturer	\N	2026-07-16 16:33:10.325371+07	2026-07-16 16:33:10.325371+07
a2954b56-b0cb-432f-9c1f-a59869779391	2e25e60a-6743-4fe9-bc89-ae5c9c733730	in_app	คำขอแต่งตั้งผู้ช่วยสอนไม่ผ่านการอนุมัติ	คำขอผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล ที่ระบุชื่อคุณไม่ผ่านการอนุมัติ: ณัฐภัทร ประชุมวงษ์ ยังไม่ได้บันทึกตารางเรียนของภาคการศึกษานี้ • ณัฐภัทร ประชุมวงษ์ ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้	/ta	\N	\N	2026-07-16 16:33:10.33097+07
f024ac37-d795-46e4-af0d-fa6ae4b29bd4	2e25e60a-6743-4fe9-bc89-ae5c9c733730	email	คำขอแต่งตั้งผู้ช่วยสอนไม่ผ่านการอนุมัติ	คำขอผู้ช่วยสอนวิชา SC362005 การวิเคราะห์และออกแบบฐานข้อมูล ที่ระบุชื่อคุณไม่ผ่านการอนุมัติ: ณัฐภัทร ประชุมวงษ์ ยังไม่ได้บันทึกตารางเรียนของภาคการศึกษานี้ • ณัฐภัทร ประชุมวงษ์ ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้	/ta	\N	2026-07-16 16:33:10.334288+07	2026-07-16 16:33:10.334288+07
ef68521e-447b-4901-b318-55225a7e69b2	8d7211e1-8a6e-469b-b9ac-653df81f83ed	in_app	บันทึกเวลาถูกปฏิเสธ	บันทึกเวลาของคุณถูกส่งกลับให้แก้ไข: ไล่ออก	/ta/worklog	\N	\N	2026-07-16 16:39:43.785002+07
f6097587-6fd8-4836-843c-71faa07efbe8	8d7211e1-8a6e-469b-b9ac-653df81f83ed	email	บันทึกเวลาถูกปฏิเสธ	บันทึกเวลาของคุณถูกส่งกลับให้แก้ไข: ไล่ออก	/ta/worklog	\N	2026-07-16 16:39:43.789147+07	2026-07-16 16:39:43.789147+07
2a7462d2-dd13-4252-87a1-400bdf9ed215	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	in_app	มีบันทึกเวลารอการอนุมัติ	TA ส่งบันทึกเวลาปฏิบัติงานรอการอนุมัติจากคุณ	/lecturer/approvals	\N	\N	2026-07-16 16:40:33.14639+07
bddf60b7-ce7d-4df0-84b5-8718252091ae	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	email	มีบันทึกเวลารอการอนุมัติ	TA ส่งบันทึกเวลาปฏิบัติงานรอการอนุมัติจากคุณ	/lecturer/approvals	\N	2026-07-16 16:40:33.150456+07	2026-07-16 16:40:33.150456+07
\.


--
-- Data for Name: pay_rates; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.pay_rates (id, effective_from, undergrad_regular, undergrad_special, graduate_regular, graduate_special_lumpsum, note, created_at, ug_lecture_hours_per_credit, ug_lab_hours_per_credit, baseline_students_lecture, baseline_students_lab, ug_workload_rate_regular, ug_workload_rate_special, term_months, ug_max_hours_per_day, max_courses_per_student, graduate_regular_hourly, grad_special_term_cap, daily_pay_cap_baht, ug_regular_daily_hour_cap, ug_special_daily_hour_cap, grad_regular_daily_hour_cap, ug_special_monthly_cap) FROM stdin;
d1d5b40b-2a07-4061-b122-3c0ce4318b89	2026-07-08	40.00	50.00	3000.00	4000.00	seed defaults from Excel workbook tab 2_59 ป.ตรี	2026-07-08 01:55:56.519521+07	3.00	4.50	60	30	200.00	250.00	4	7	3	50.00	12000.00	300.00	7.00	6.00	6.00	2000.00
c15e2e2a-4910-48fb-a30b-5dd468c5c08d	2026-07-07	40.00	50.00	50.00	4000.00	seed defaults from Excel workbook tab 2_59 ป.ตรี	2026-07-08 03:41:32.172993+07	3.00	4.50	60	30	200.00	250.00	4	7	3	50.00	12000.00	300.00	7.00	6.00	6.00	2000.00
0c938ba4-6292-4c2b-8b8a-97626862921c	2026-07-10	40.00	50.00	50.00	4000.00	seed defaults from Excel workbook tab 2_59 ป.ตรี	2026-07-10 10:31:47.667216+07	3.00	4.50	60	30	200.00	250.00	4	7	3	50.00	12000.00	300.00	7.00	6.00	6.00	2000.00
\.


--
-- Data for Name: public_holidays; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.public_holidays (id, holiday_date, name_th, name_en, source, note, created_by, created_at) FROM stdin;
941231c8-e8a0-4598-8b74-b2a45cefb23a	2026-08-12	วันแม่แห่งชาติ / วันเฉลิมพระชนมพรรษาสมเด็จพระบรมราชชนนีพันปีหลวง	H.M. Queen Sirikit's Birthday / Mother's Day	national	\N	\N	2026-07-15 06:28:48.096312+07
481bf7d2-0804-47c6-a30b-0f3bc0f255c9	2026-10-13	วันคล้ายวันสวรรคต ร.9	H.M. King Bhumibol Adulyadej Memorial Day	national	\N	\N	2026-07-15 06:28:48.096312+07
d2265ad2-9064-444f-bb9a-74eafab2d7ce	2026-10-23	วันปิยมหาราช	Chulalongkorn Day	national	\N	\N	2026-07-15 06:28:48.096312+07
7854aa7c-5cb3-4730-96cd-f5ee4b406b35	2026-12-05	วันพ่อแห่งชาติ / วันคล้ายวันพระราชสมภพ ร.9 / วันชาติ	H.M. King Bhumibol Adulyadej's Birthday / National Day / Father's Day	national	\N	\N	2026-07-15 06:28:48.096312+07
b890c1df-3716-4ee5-a670-c84be65d6758	2026-12-10	วันรัฐธรรมนูญ	Constitution Day	national	\N	\N	2026-07-15 06:28:48.096312+07
052499de-2344-433c-9146-ceb180978c7b	2026-12-31	วันสิ้นปี	New Year's Eve	national	\N	\N	2026-07-15 06:28:48.096312+07
9b5ec879-2a07-4460-8ab5-3ba873fd09a3	2027-01-01	วันขึ้นปีใหม่	New Year's Day	national	\N	\N	2026-07-15 06:28:48.096312+07
f505c026-d0c5-4d0e-8a02-78c74e756604	2027-04-06	วันจักรี	Chakri Memorial Day	national	\N	\N	2026-07-15 06:28:48.096312+07
a85e0f81-8da6-4669-bcd6-e4199e21d833	2027-04-13	วันสงกรานต์	Songkran Festival	national	\N	\N	2026-07-15 06:28:48.096312+07
0a864e43-7c17-4997-8470-b6f8643b763c	2027-04-14	วันสงกรานต์	Songkran Festival	national	\N	\N	2026-07-15 06:28:48.096312+07
3a405392-3715-496d-9aed-38c334b0eb72	2027-04-15	วันสงกรานต์	Songkran Festival	national	\N	\N	2026-07-15 06:28:48.096312+07
3c67d699-38eb-43fe-ba12-3826ae1152f7	2027-05-01	วันแรงงานแห่งชาติ	National Labour Day	national	\N	\N	2026-07-15 06:28:48.096312+07
17529c16-db62-4b4c-b1ae-83fe49143e5a	2027-05-04	วันฉัตรมงคล	Coronation Day	national	\N	\N	2026-07-15 06:28:48.096312+07
79637220-4637-4f97-be31-2d901ee8c0bc	2027-06-03	วันเฉลิมพระชนมพรรษาสมเด็จพระราชินี	H.M. Queen Suthida's Birthday	national	\N	\N	2026-07-15 06:28:48.096312+07
eaaf89e0-3196-4c3a-99ae-bec1394e99d7	2027-07-28	วันเฉลิมพระชนมพรรษา ร.10	H.M. King Vajiralongkorn's Birthday	national	\N	\N	2026-07-15 06:28:48.096312+07
afc9d4e2-08e5-43f1-831d-d3266c1e2d5c	2027-08-12	วันแม่แห่งชาติ / วันเฉลิมพระชนมพรรษาสมเด็จพระบรมราชชนนีพันปีหลวง	H.M. Queen Sirikit's Birthday / Mother's Day	national	\N	\N	2026-07-15 06:28:48.096312+07
ec87a3b5-522c-4750-8b13-11f2fb63de7f	2027-10-13	วันคล้ายวันสวรรคต ร.9	H.M. King Bhumibol Adulyadej Memorial Day	national	\N	\N	2026-07-15 06:28:48.096312+07
1ff06723-f93d-4b39-a171-9cff1308180e	2027-10-23	วันปิยมหาราช	Chulalongkorn Day	national	\N	\N	2026-07-15 06:28:48.096312+07
e1857cfb-d91d-434f-b22a-826cfdc09169	2027-12-05	วันพ่อแห่งชาติ / วันคล้ายวันพระราชสมภพ ร.9 / วันชาติ	H.M. King Bhumibol Adulyadej's Birthday / National Day / Father's Day	national	\N	\N	2026-07-15 06:28:48.096312+07
4f362218-9357-453c-81a3-8b4196c20c90	2027-12-10	วันรัฐธรรมนูญ	Constitution Day	national	\N	\N	2026-07-15 06:28:48.096312+07
f67cbf50-918d-440f-8a32-3db4d3366f01	2027-12-31	วันสิ้นปี	New Year's Eve	national	\N	\N	2026-07-15 06:28:48.096312+07
8ae5de5a-3fe8-4536-a043-a8854506986b	2026-01-01	วันขึ้นปีใหม่	New Year’s Day	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
61d1afb7-9a9d-4363-a17c-5f0dccc8a321	2026-01-02	วันหยุดทำการเพิ่มเป็นกรณีพิเศษ	Additional special holiday	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
69c77714-d5f2-46f2-8eae-48009c2a4e8d	2026-03-03	วันมาฆบูชา	Makha Bucha Day	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
ba954556-191c-44a3-adf4-9d9602e89229	2026-04-06	วันพระบาทสมเด็จพระพุทธยอดฟ้าจุฬาโลกมหาราช และวันที่ระลึกมหาจักรีบรมราชวงศ์	Chakri Memorial Day	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
1eab08e5-cf57-4ac8-8b8f-203d2743083f	2026-04-13	วันสงกรานต์	Songkran Festival	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
960ad95f-b371-4128-9250-27ead568d357	2026-04-14	วันสงกรานต์	Songkran Festival	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
44bd5ea5-c0ac-40bb-8971-08611b996aaf	2026-04-15	วันสงกรานต์	Songkran Festival	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
60b4a416-fa46-47cf-9c7c-9702c416d160	2026-05-01	วันแรงงานแห่งชาติ	National Labor Day	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
8c39d45c-95ed-4561-95ba-538591b1f2c0	2026-05-04	วันฉัตรมงคล	Coronation Day	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
545ed18d-ae63-4f12-8df8-2b029210689a	2026-06-01	ชดเชยวันวิสาขบูชา (วันอาทิตย์ที่ 31 พฤษภาคม 2569)	Substitution for Visakha Bucha Day (Sunday 31st May 2026)	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
91ffe603-2f53-4a63-9cd2-07c21d7e9c02	2026-06-03	วันเฉลิมพระชนมพรรษาสมเด็จพระนางเจ้าสุทิดา พัชรสุธาพิมลลักษณ พระบรมราชินี	H.M. Queen Suthida Bajrasudhabimalalakshana’s Birthday	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
e2c55d70-ca30-426c-9295-6277e1c34a3b	2026-07-28	วันเฉลิมพระชนมพรรษาพระบาทสมเด็จพระเจ้าอยู่หัว	H.M. King Maha Vajiralongkorn Phra Vajiraklaochaoyuhua’s Birthday	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
2732aced-ac58-4a72-afdf-87ae6175165e	2026-07-29	วันอาสาฬหบูชา	Asarnha Bucha Day	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
fa1910f6-18a5-4369-a2fd-33555a1144cf	2026-12-07	ชดเชยวันคล้ายวันพระบรมราชสมภพ พระบาทสมเด็จพระบรมชนกาธิเบศร มหาภูมิพลอดุลยเดชมหาราช บรมนาถบพิตร วันชาติ และวันพ่อแห่งชาติ (วันเสาร์ที่ 5 ธันวาคม 2569)	Substitution for H.M. King Bhumibol Adulyadej the Great’s Birthday, National Day and Father’s Day (Saturday 5th December 2026)	national	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-15 08:04:09.26749+07
\.


--
-- Data for Name: schedule_imports; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.schedule_imports (id, imported_by, filename, row_count, error_count, summary, at) FROM stdin;
8bb5f86e-90c0-4054-a5c0-a5674d697de9	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	รายวิชาที่เปิดสอน-1-2569.xlsx	127	65	{"row_count": 127, "error_count": 65, "created_count": 127, "skipped_count": 0}	2026-07-23 17:29:07.259362+07
\.


--
-- Data for Name: schema_migrations; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.schema_migrations (version, applied_at) FROM stdin;
0001_init	2026-07-08 01:55:55.734885+07
0002_signature_and_workload_rates	2026-07-08 01:55:56.438845+07
0003_pay_rate_policy_limits	2026-07-08 05:18:11.49318+07
0004_term_months	2026-07-08 05:18:11.521586+07
0005_align_ug_workload_rate	2026-07-08 05:18:11.528323+07
0006_user_title_and_password_change	2026-07-08 07:40:53.943012+07
0007_course_exam_dates	2026-07-08 08:51:04.812082+07
0008_section_num_students_and_export_lock	2026-07-08 09:27:24.072556+07
0009_ta_docs_history	2026-07-09 05:45:01.473348+07
0010_ta_profile_prefix	2026-07-09 07:47:21.350978+07
0011_ta_class_schedule_fields	2026-07-09 18:32:47.023879+07
0012_announcement_extras	2026-07-10 01:58:41.640518+07
0013_user_study_year	2026-07-10 01:58:41.743031+07
0014_admin_officers	2026-07-10 01:58:41.750413+07
0015_admin_officers_drop_sort_order	2026-07-10 02:27:17.733881+07
0016_ta_docs_retention	2026-07-12 11:20:03.581972+07
0017_ta_request_auto_decide	2026-07-12 12:04:33.188111+07
0018_business_rules_2026	2026-07-13 03:33:02.39789+07
0019_monthly_submission_periods	2026-07-13 07:10:04.442109+07
0020_export_batches	2026-07-13 07:10:04.538441+07
0021_drop_hour_caps	2026-07-14 03:12:20.581672+07
0022_term_exam_periods	2026-07-14 04:35:30.250511+07
0023_public_holidays	2026-07-15 06:28:48.002372+07
0024_seed_holidays_th	2026-07-15 06:28:48.096312+07
0025_ta_review_schedules	2026-07-15 07:01:30.124653+07
0026_submission_period_timeline	2026-07-15 13:38:25.838657+07
0027_submission_periods_starts_on	2026-07-15 22:51:51.584532+07
0028_submission_sendback_and_lock	2026-07-15 22:51:51.681397+07
0029_submission_flow_no_signatures	2026-07-16 01:31:58.685443+07
0030_document_progress	2026-07-16 03:56:06.889338+07
0031_document_progress_per_term	2026-07-16 04:14:03.679309+07
0032_faculty_course_level	2026-07-17 08:47:12.95833+07
0033_admin_officer_full_prefix	2026-07-17 09:33:46.940046+07
0034_fix_academic_title_order	2026-07-23 13:25:36.180689+07
0035_holiday_faculty_source	2026-07-23 13:25:36.210403+07
0036_ta_request_is_late	2026-07-23 13:25:36.327512+07
0037_signature_checklist	2026-07-23 14:18:35.330874+07
0038_scrub_national_id	2026-07-23 14:30:25.42714+07
0039_denormalize_teaching_courses	2026-07-23 17:03:39.953338+07
0040_ug_special_monthly_cap	2026-07-24 08:41:41.604107+07
\.


--
-- Data for Name: section_schedules; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.section_schedules (id, section_id, kind, day_of_week, start_time, end_time, room) FROM stdin;
f698c431-52f9-4af0-87aa-1d555d8c310f	13a54f98-c08c-453e-adcc-dc9971b07ba1	lecture	1	09:00:00	12:00:00	\N
6e17b787-0997-4502-81b6-8be3f3943f14	13a54f98-c08c-453e-adcc-dc9971b07ba1	lab	2	09:00:00	12:00:00	\N
c3e81d5b-1cc5-42c1-a4cc-71d615fa5d67	95f12def-b084-4e87-8532-530a6deeef70	lecture	1	09:00:00	12:00:00	\N
99327407-0a64-47db-9671-3c6c604f257f	95f12def-b084-4e87-8532-530a6deeef70	lab	3	09:00:00	12:00:00	\N
d0546f2a-68c2-49d2-b40f-854b0a1c435e	aeb5e366-7da6-4286-a823-c878c1c903fb	lecture	5	08:30:00	10:30:00	\N
9fc92be1-8899-4ffa-bd8a-1f8c185abe68	aeb5e366-7da6-4286-a823-c878c1c903fb	lab	5	10:30:00	12:30:00	\N
7cab257e-47ce-44bf-b908-b63c82219405	db603f69-2872-48a1-8d7f-a9dafebbd0dd	lecture	4	15:00:00	17:00:00	\N
39abc466-41e1-4f7d-a327-74883bff43b4	db603f69-2872-48a1-8d7f-a9dafebbd0dd	lab	4	17:00:00	19:00:00	\N
e3cabb1e-d59f-4707-89f4-a76a121e1dfa	47c9d7aa-35f5-4f4c-a833-2d8ea3593c48	lecture	4	10:30:00	12:30:00	\N
99b59a4b-bab7-4ca0-b700-19845a55aca5	47c9d7aa-35f5-4f4c-a833-2d8ea3593c48	lab	4	13:00:00	15:00:00	\N
ca10f723-a8e1-4bc9-88be-8eb617870631	64acb93b-4903-42f9-9595-873ae34770fe	lecture	2	13:00:00	15:00:00	\N
48bdb4a9-ef48-4c12-b808-36d02af49f5f	64acb93b-4903-42f9-9595-873ae34770fe	lab	5	13:00:00	15:00:00	\N
244484d5-a284-42a8-be2d-7e397c1cb095	c98a61c8-939d-4456-b938-bb86ce7bac87	lecture	2	13:00:00	15:00:00	\N
203092e0-c910-499e-9a85-68027e43a7f7	c98a61c8-939d-4456-b938-bb86ce7bac87	lab	5	15:00:00	17:00:00	\N
2f87d7b8-3b3f-4121-809e-71c9a51319bb	94733a8a-fb8a-41b3-b386-7d4cd5b3205b	lecture	2	13:00:00	15:00:00	\N
e234553d-5fb4-45ef-8992-1069330ffd14	94733a8a-fb8a-41b3-b386-7d4cd5b3205b	lab	5	15:00:00	17:00:00	\N
cbb38cb9-fa7e-44e7-b62e-02f12d8d295e	1cda43c8-8219-47a0-9ab3-922ccdc32eea	lecture	2	17:00:00	19:00:00	\N
4a67fd8f-2949-465c-99e5-5d0371ab718d	1cda43c8-8219-47a0-9ab3-922ccdc32eea	lab	4	13:00:00	15:00:00	\N
e21328d5-1bb3-4b94-bf82-f70002034f82	05af3a00-e25d-4fd1-aef5-ad41334ff4e8	lecture	2	17:00:00	19:00:00	\N
69a78235-4c5b-4f0b-be35-971811b697bf	05af3a00-e25d-4fd1-aef5-ad41334ff4e8	lab	4	13:00:00	15:00:00	\N
fad7bea9-9f1f-47ff-bf6f-dfa8e4be5abf	f42b9667-a261-43e6-8dc9-4a6ee3c8abed	lecture	1	13:00:00	15:00:00	\N
3a6073c6-83eb-4ac7-9528-d0d57ed0a686	f42b9667-a261-43e6-8dc9-4a6ee3c8abed	lab	1	15:00:00	17:00:00	\N
f2d8023e-fb3c-4945-9d56-873828d0e72f	9d9ce393-9cef-429d-afd4-3b0ef7229644	lecture	1	13:00:00	15:00:00	\N
c9402d09-ece6-4e9e-bebe-251168feded4	9d9ce393-9cef-429d-afd4-3b0ef7229644	lab	1	17:00:00	19:00:00	\N
86c15ad7-8b1c-48f2-a462-8655782166c9	ec11dd51-2af3-40da-afa9-8703523b837e	lecture	3	10:30:00	12:30:00	\N
158fdf2d-893e-446e-9e95-16c64f6d0736	ec11dd51-2af3-40da-afa9-8703523b837e	lab	3	13:00:00	15:00:00	\N
c4abb342-3cc1-4d51-b808-3611edc221b7	80129309-def2-47ee-a51a-423fdcbd0808	lecture	3	10:30:00	12:30:00	\N
a070ee34-4c4d-4c8d-8a7b-7723962c2178	80129309-def2-47ee-a51a-423fdcbd0808	lab	3	13:00:00	15:00:00	\N
06916e00-1828-415f-9e0e-c78a7d2d173f	130f437e-714e-4959-923d-266135e0ae42	lecture	3	10:30:00	12:30:00	\N
b91de481-94fd-459f-9f6a-27120849a1b1	130f437e-714e-4959-923d-266135e0ae42	lab	3	13:00:00	15:00:00	\N
\.


--
-- Data for Name: sections; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.sections (id, teaching_course_id, sec_no, track, room, num_students) FROM stdin;
13a54f98-c08c-453e-adcc-dc9971b07ba1	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	1	regular	\N	40
95f12def-b084-4e87-8532-530a6deeef70	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	2	special	\N	23
aeb5e366-7da6-4286-a823-c878c1c903fb	1c0b6bb3-87ec-43ff-9b63-ff0a491ad4d7	1	regular	\N	0
db603f69-2872-48a1-8d7f-a9dafebbd0dd	1c0b6bb3-87ec-43ff-9b63-ff0a491ad4d7	2	special	\N	0
47c9d7aa-35f5-4f4c-a833-2d8ea3593c48	a416b931-2506-45a8-96c7-d06de9246ff7	1	regular	\N	0
64acb93b-4903-42f9-9595-873ae34770fe	1a656edf-6a1b-4de1-b441-51e02d3150de	1	regular	\N	0
c98a61c8-939d-4456-b938-bb86ce7bac87	1a656edf-6a1b-4de1-b441-51e02d3150de	2	regular	\N	0
94733a8a-fb8a-41b3-b386-7d4cd5b3205b	1a656edf-6a1b-4de1-b441-51e02d3150de	3	special	\N	0
1cda43c8-8219-47a0-9ab3-922ccdc32eea	cea72167-38ab-43c8-b80c-8a4b964c90c2	1	regular	\N	0
05af3a00-e25d-4fd1-aef5-ad41334ff4e8	cea72167-38ab-43c8-b80c-8a4b964c90c2	2	special	\N	0
f42b9667-a261-43e6-8dc9-4a6ee3c8abed	ad611905-ad80-4d20-8fba-86339488b093	1	regular	\N	0
9d9ce393-9cef-429d-afd4-3b0ef7229644	ad611905-ad80-4d20-8fba-86339488b093	2	regular	\N	0
ec11dd51-2af3-40da-afa9-8703523b837e	ad4b1cff-c174-477c-827b-0a6f63e4beb8	1	regular	\N	0
80129309-def2-47ee-a51a-423fdcbd0808	ad4b1cff-c174-477c-827b-0a6f63e4beb8	2	regular	\N	0
130f437e-714e-4959-923d-266135e0ae42	ad4b1cff-c174-477c-827b-0a6f63e4beb8	3	special	\N	0
\.


--
-- Data for Name: signature_checklist; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.signature_checklist (id, term_id, teaching_course_id, role, signed_at, updated_by, updated_by_name, updated_at) FROM stdin;
77254a08-23e8-430f-88a4-ee318a0e81b0	2a01f439-a013-4f5f-a819-5ef591497243	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	ta	\N	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	Admin COCO	2026-07-23 14:20:46.8214+07
\.


--
-- Data for Name: submission_period_status; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.submission_period_status (id, submission_period_id, ta_id, teaching_course_id, status, ta_signed_at, lecturer_signed_at, submitted_at, last_reminded_at, created_at, ta_signed_name, lecturer_signed_by, lecturer_signed_name, lecturer_comment, staff_reviewed_by, staff_reviewed_name, staff_comment, finance_sent_at, finance_sent_by, finance_sent_name, finance_note, sent_back_at, sent_back_by, sent_back_name, sent_back_reason, exported_at, exported_by, exported_name) FROM stdin;
\.


--
-- Data for Name: submission_periods; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.submission_periods (id, term_id, year_month, due_date, label, remind_days_before, is_closed, created_at, starts_on) FROM stdin;
1cb6e7c0-5d40-400a-b7f6-68b833ae46ed	2a01f439-a013-4f5f-a819-5ef591497243	2569-06	2026-07-31	มิถุนายน 2569	3	f	2026-07-15 14:20:02.758299+07	2026-06-01
220e35af-de73-4127-9c19-123bcc7232b2	2a01f439-a013-4f5f-a819-5ef591497243	2569-07	2026-07-31	กรกฎาคม 2569	3	f	2026-07-15 14:20:02.778752+07	2026-07-01
09f23903-05a9-4b17-9bbb-03a022356092	2a01f439-a013-4f5f-a819-5ef591497243	2569-09	2026-10-05	กันยายน 2569	3	f	2026-07-15 14:20:02.786002+07	2026-09-01
36454455-6ea3-4684-8670-b338c5ed74c6	2a01f439-a013-4f5f-a819-5ef591497243	2569-10	2026-11-05	ตุลาคม 2569	3	f	2026-07-15 14:20:02.789316+07	2026-10-01
f608c541-89a6-4da9-8af5-53070944d460	2a01f439-a013-4f5f-a819-5ef591497243	2569-08	2026-07-31	สิงหาคม 2569	3	f	2026-07-15 14:20:02.781708+07	2026-07-01
\.


--
-- Data for Name: ta_class_schedules; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.ta_class_schedules (id, user_id, term_id, course_label, day_of_week, start_time, end_time, note, is_wba, course_code, course_name, kind, sec_no) FROM stdin;
29fc7f74-0d2f-4ecc-a14c-14256f6e8cc7	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	2a01f439-a013-4f5f-a819-5ef591497243	\N	4	12:00:00	14:00:00		f	CP410872	วิศวกรรมออนโทโลยีสําหรับกราฟความรู้	lecture	1
883ab012-6314-4337-b495-70f3ab049155	67959d3a-87dc-476f-ab3e-0ce6c054a444	2a01f439-a013-4f5f-a819-5ef591497243	\N	1	09:00:00	11:00:00		f	CP321002	การเขียนโปรแกรมเชิงโครงสร้างสาหรับเทคโนโลยีสารสนเทศ	lecture	1
4ea74d7b-3949-418e-be80-67315f474742	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	2a01f439-a013-4f5f-a819-5ef591497243	\N	4	09:00:00	11:00:00		f	CP321003	แนวคิดและการเขียนโปรแกรมเชิงวัตถุ	lecture	1
cf0002bf-13a3-4bc7-86aa-0693ed88942c	8d7211e1-8a6e-469b-b9ac-653df81f83ed	2a01f439-a013-4f5f-a819-5ef591497243	\N	0	00:00:00	00:00:00	ไม่มีตารางเรียนปกติ	t	\N	WBA / ปี 4	\N	\N
\.


--
-- Data for Name: ta_documents; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.ta_documents (id, user_id, kind, filename, mime, size_bytes, storage_key, status, reject_reason, uploaded_at, reviewed_at, reviewed_by, superseded_at, superseded_by, round, expires_at, file_deleted_at, reject_batch_id) FROM stdin;
c3ca4679-8ab3-4194-879f-39b464b95ebd	b134e943-7410-44fd-883b-0b32f4a93b33	creditor_form	creditor_form_สุพพิธาน_ภักสวัสดิ์.pdf	application/pdf	162204	ta_docs/2026/07/09/3a636d91-bf4d-409d-8554-6540750f9ca1.pdf.enc	approved	\N	2026-07-09 09:24:16.964164+07	2026-07-09 09:55:17.005881+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	\N	\N	\N
e839f896-1e74-44e7-8d68-0ded9c7e515d	b134e943-7410-44fd-883b-0b32f4a93b33	national_id	733499934_3640200259468202_4940892661214164506_n.jpg	image/jpeg	238026	ta_docs/2026/07/09/f560f740-e27e-4ed2-9757-913afc4458c4.jpg.enc	approved	\N	2026-07-09 09:40:44.970861+07	2026-07-09 09:55:19.288872+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	\N	\N	\N
b0e8d729-307c-464c-a8c1-bf2707db5f08	b134e943-7410-44fd-883b-0b32f4a93b33	bank_book	733499934_3640200259468202_4940892661214164506_n.jpg	image/jpeg	238026	ta_docs/2026/07/09/a2cb9a0d-2c51-420f-8dcf-34e9782574cd.jpg.enc	rejected	ส่งผิดภาพ	2026-07-09 09:43:57.766332+07	2026-07-09 09:55:30.99002+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-09 10:06:27.695409+07	3a6b798d-a81c-4737-844b-de4dd322b77a	1	\N	\N	\N
3a6b798d-a81c-4737-844b-de4dd322b77a	b134e943-7410-44fd-883b-0b32f4a93b33	bank_book	727800449_1544583450671199_713885289005163738_n.jpg	image/jpeg	157961	ta_docs/2026/07/09/ee689eaa-5c9e-4816-a4d5-274e44473a78.jpg.enc	approved	\N	2026-07-09 10:06:27.695409+07	2026-07-09 10:06:46.294367+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	2	\N	\N	\N
77b1ef5c-ddf0-43f4-8205-dc98ad371a89	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	bank_book	JanJingJing_1.jpg	image/jpeg	296941	ta_docs/2026/07/09/d782230e-bf1d-4dfb-95b0-6220494ac09d.jpg.enc	approved	\N	2026-07-09 19:15:42.39484+07	2026-07-09 19:16:31.278375+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	\N	\N	\N
9221344f-fe75-4d4b-8436-fe5551567575	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	national_id	images.jpg	image/jpeg	11935	ta_docs/2026/07/09/e8363d17-1ded-4836-9cf3-3b7b015e1acd.jpg.enc	approved	\N	2026-07-09 19:14:53.254819+07	2026-07-09 19:16:31.895873+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	\N	\N	\N
d3768244-84cd-4e3f-84d5-dc20d55f7d58	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	creditor_form	creditor_form_วรพจน์_สุวรรณภิภพ.pdf	application/pdf	160348	ta_docs/2026/07/09/fb06a16a-c4a6-4cdb-b02a-a42b84e21b33.pdf.enc	approved	\N	2026-07-09 19:14:34.119576+07	2026-07-09 19:16:32.740214+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	\N	\N	\N
06babb4b-f242-47dc-8537-95fedccd6b1d	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	creditor_form	creditor_form_ธนเดช_วาตรีบุญเรือง.pdf	application/pdf	160961	ta_docs/2026/07/14/1a6b7cbb-21b5-444a-8f25-1886afb54760.pdf.enc	submitted	\N	2026-07-14 02:38:57.79672+07	\N	\N	\N	\N	1	\N	\N	\N
410f8e9c-8fe2-4ab1-b837-bce68b39a4a9	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	bank_book	733499934_3640200259468202_4940892661214164506_n.jpg	image/jpeg	238026	ta_docs/2026/07/14/c5d8ff78-a50e-4916-9b7b-8bc65a444e00.jpg.enc	submitted	\N	2026-07-14 02:39:16.891399+07	\N	\N	\N	\N	1	\N	\N	\N
f6bca7f2-89cf-4ca6-9ae4-e95ec6adab3b	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	national_id	733499934_3640200259468202_4940892661214164506_n.jpg	image/jpeg	238026	ta_docs/2026/07/14/e5e996da-878f-4485-bf2f-1e3fcdcc1726.jpg.enc	submitted	\N	2026-07-14 02:39:19.387599+07	\N	\N	\N	\N	1	\N	\N	\N
dc46188f-67fa-48b3-9be0-906b74a21eed	67959d3a-87dc-476f-ab3e-0ce6c054a444	national_id	733499934_3640200259468202_4940892661214164506_n.jpg	image/jpeg	238026	ta_docs/2026/07/14/1daa64f1-39da-469d-a8f2-1792b3cb7d6b.jpg.enc	approved	\N	2026-07-14 02:16:07.040537+07	2026-07-17 10:47:45.029768+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	2026-07-24 10:47:45.029768+07	\N	b4272d60-f4c2-4123-b735-c9a8785b1505
f85dccda-af40-4e1b-8b03-1d30a6084f10	67959d3a-87dc-476f-ab3e-0ce6c054a444	creditor_form	creditor_form_จุฑามาศ_ชะรานันท์.pdf	application/pdf	160736	ta_docs/2026/07/14/bd273c82-0cf3-4b51-a023-c7fabf9d9049.pdf.enc	approved	\N	2026-07-14 02:15:58.582704+07	2026-07-17 10:47:45.029768+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	2026-07-24 10:47:45.029768+07	\N	\N
fbfb19fc-ff2f-4fda-94f9-305a5f8c5b8d	67959d3a-87dc-476f-ab3e-0ce6c054a444	bank_book	733499934_3640200259468202_4940892661214164506_n.jpg	image/jpeg	238026	ta_docs/2026/07/14/20c3ba55-90bf-40a5-ae8d-2205bad625c9.jpg.enc	approved	\N	2026-07-14 02:16:17.655071+07	2026-07-17 10:47:45.029768+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	2026-07-24 10:47:45.029768+07	\N	\N
bf86c84a-6611-4d14-8e32-0555b5ec4cf3	8d7211e1-8a6e-469b-b9ac-653df81f83ed	creditor_form	creditor_form_ภัทรวดี_วงศ์นอก.pdf	application/pdf	175416	ta_docs/2026/07/16/b7c1d556-b8c3-44aa-8291-d481719653cf.pdf.enc	approved	\N	2026-07-16 16:22:44.647032+07	2026-07-16 16:26:40.877647+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	2026-07-23 16:26:40.877647+07	2026-07-23 16:33:33.637568+07	\N
ada63b7f-0d93-4f77-9718-d00c9350ba20	8d7211e1-8a6e-469b-b9ac-653df81f83ed	national_id	IMG_1054.jpeg	image/jpeg	202062	ta_docs/2026/07/16/5a75ca88-71d9-4f7e-9e29-f4813dbcaf93.jpeg.enc	approved	\N	2026-07-16 16:23:04.148876+07	2026-07-16 16:26:40.877647+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	2026-07-23 16:26:40.877647+07	2026-07-23 16:33:33.649295+07	\N
696f343a-0ada-4e69-8e2d-03d0ed46e86f	8d7211e1-8a6e-469b-b9ac-653df81f83ed	bank_book	IMG_0853.jpeg	image/jpeg	72873	ta_docs/2026/07/16/ed8917bf-47df-4bef-99ca-50a572d56e6b.jpeg.enc	approved	\N	2026-07-16 16:23:44.111461+07	2026-07-16 16:26:40.877647+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	\N	1	2026-07-23 16:26:40.877647+07	2026-07-23 16:33:33.663473+07	\N
\.


--
-- Data for Name: ta_profile_submissions; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.ta_profile_submissions (id, user_id, round, national_id, bank_name, bank_branch, branch_code, account_no, account_name, signature_svg, submitted_at, status, reviewed_at, reviewed_by, reject_reason, prefix) FROM stdin;
db6c962f-8e9d-424e-951b-bb02333b74e3	b134e943-7410-44fd-883b-0b32f4a93b33	1	\N	ธนาคารไทยพาณิชย์	เทสโก้ โลตัส หนองบัวลำภู	5311	4091290303	นายสุพพิธาน ภักสวัสดิ์	<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 140'><path d='M 161.0 94.4 L 161.0 93.4 L 162.0 92.4 L 166.0 88.4 L 179.0 73.4 L 184.0 70.4 L 192.0 68.4 L 201.0 67.4 L 209.0 67.4 L 219.0 70.4 L 220.0 71.4 L 222.0 73.4 L 223.0 74.4 L 225.0 77.4 L 226.0 79.4 L 231.0 84.4 L 232.0 85.4 L 233.0 85.4 L 233.0 82.4 L 233.0 79.4 L 231.0 76.4 L 230.0 72.4 L 219.0 50.4 L 216.0 45.4 L 211.0 39.4 L 208.0 36.4 L 207.0 35.4 L 203.0 34.4 L 196.0 34.4 L 184.0 39.4 L 171.0 46.4 L 164.0 54.4 L 162.0 56.4 L 159.0 62.4 L 157.0 70.4 L 156.0 74.4 L 156.0 76.4 L 156.0 77.4 M 181.0 77.4 L 182.0 76.4 L 187.0 71.4 L 197.0 61.4 L 224.0 39.4 L 239.0 30.4 L 245.0 27.4 L 248.0 24.4' stroke='#111' stroke-width='2' fill='none' stroke-linejoin='round' stroke-linecap='round'/></svg>	2026-07-09 09:21:30.751553+07	approved	2026-07-09 10:06:49.322967+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	นาย
a52c9b4c-dddb-49d3-bf48-eefd322f8ab2	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	1	\N	ธนาคารไทยพาณิชย์	ขอนแก่น	0555	1234567890	นายวรพจน์ สุวรรณภิภพ	<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 140'><path d='M 256.0 95.8 L 255.0 95.8 L 254.0 95.8 L 251.0 95.8 L 250.0 95.8 L 250.0 96.8 L 250.0 99.8 L 250.0 108.8 L 251.0 126.8 L 252.0 128.8 L 255.0 130.8 L 260.0 131.8 L 265.0 131.8 L 281.0 127.8 L 281.0 126.8 L 281.0 122.8 L 282.0 116.8 L 282.0 92.8 L 282.0 77.8 L 282.0 73.8 L 281.0 69.8 L 274.0 57.8 L 271.0 53.8 L 266.0 47.8 L 261.0 45.8 L 249.0 44.8 L 240.0 47.8 L 216.0 60.8 L 209.0 65.8 L 196.0 78.8' stroke='#111' stroke-width='2' fill='none' stroke-linejoin='round' stroke-linecap='round'/></svg>	2026-07-09 19:14:25.678753+07	approved	2026-07-09 19:16:35.921298+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	นาย
ebc5b6c6-6674-4729-910d-6feaf136b8c7	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	1	\N	ธนาคารทหารไทยธนชาต	ขอนแก่น	6789	1234567890	นายธนเดช วาตรีบุญเรือง	<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 140'><path d='M 205.0 74.8 L 204.0 73.8 L 198.0 65.8 L 195.0 60.8 L 189.0 52.8 L 188.0 50.8 L 189.0 49.8 L 206.0 58.8 L 212.0 62.8 L 227.0 77.8 L 233.0 88.8 L 238.0 98.8 L 240.0 103.8 L 240.0 104.8 L 241.0 103.8 L 241.0 97.8 L 240.0 84.8 L 235.0 64.8 L 233.0 55.8 L 228.0 39.8 L 227.0 37.8 L 226.0 37.8 L 225.0 37.8 L 215.0 37.8 L 205.0 38.8 L 175.0 44.8 L 168.0 47.8 L 167.0 48.8 L 167.0 49.8' stroke='#111' stroke-width='2' fill='none' stroke-linejoin='round' stroke-linecap='round'/></svg>	2026-07-14 02:38:24.726099+07	submitted	\N	\N	\N	นาย
258a577a-a04f-4949-8b6f-a42974e45080	67959d3a-87dc-476f-ab3e-0ce6c054a444	1	\N	ธนาคารไทยพาณิชย์	ขอนแก่น	1234	1234567890	นางสาวจุฑามาศ  ชะรานันท์	<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 140'><path d='M 229.0 78.8 L 227.0 78.8 L 224.0 78.8 L 221.0 78.8 L 217.0 77.8 L 214.0 75.8 L 213.0 74.8 L 212.0 72.8 L 212.0 71.8 L 213.0 71.8 L 214.0 71.8 L 217.0 71.8 L 220.0 71.8 L 224.0 71.8 L 228.0 71.8 L 232.0 71.8 L 237.0 72.8 L 238.0 73.8 L 238.0 74.8 L 239.0 76.8 L 240.0 79.8 L 245.0 88.8 L 247.0 93.8 L 249.0 97.8 L 251.0 101.8 L 252.0 104.8 L 253.0 104.8 L 253.0 105.8 L 253.0 104.8 L 253.0 103.8 L 253.0 102.8 L 253.0 101.8 L 252.0 97.8 L 251.0 92.8 L 249.0 85.8 L 247.0 77.8 L 245.0 68.8 L 244.0 59.8 L 242.0 53.8 L 241.0 48.8 L 240.0 45.8 L 239.0 44.8 L 238.0 44.8 L 237.0 44.8 L 236.0 44.8 L 233.0 45.8 L 225.0 50.8 L 213.0 57.8 L 201.0 65.8 L 191.0 72.8 L 185.0 77.8 L 180.0 82.8 L 176.0 87.8' stroke='#111' stroke-width='2' fill='none' stroke-linejoin='round' stroke-linecap='round'/></svg>	2026-07-14 02:15:04.694293+07	approved	2026-07-17 10:47:45.029768+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	นางสาว
b769cfbf-6f1d-46e7-9ae2-68e4423742d4	8d7211e1-8a6e-469b-b9ac-653df81f83ed	1	\N	ธนาคารกสิกรไทย	นครราชสีมา	4567	2131515456	นางสาวภัทรวดี วงศ์นอก	<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 140'><path d='M 62.5 105.0 M 105.0 104.5 L 99.5 96.0 L 99.5 96.0 L 97.5 95.0 L 94.5 94.5 L 91.0 95.5 L 88.5 101.5 L 88.0 107.0 L 88.0 112.0 L 90.5 114.5 L 95.0 114.5 L 101.5 114.5 L 107.0 108.5 L 112.0 101.0 L 116.0 93.5 L 117.0 87.5 L 117.0 84.5 L 112.0 82.0 L 107.5 80.5 L 105.0 78.5 L 102.5 77.0 L 102.5 76.0 L 102.5 75.5 L 105.0 74.5 L 111.5 73.5 L 119.0 73.0 L 126.0 73.0 L 131.5 73.0 L 134.5 77.5 L 135.5 83.5 L 135.5 90.0 L 134.0 96.0 L 132.0 101.0 L 131.5 104.5 L 131.5 107.0 L 131.5 109.5 M 134.0 55.0 L 130.5 51.5 L 130.5 51.5 L 131.5 45.5 L 131.5 41.0 L 131.5 39.0 L 130.0 38.5 L 127.5 38.5 L 126.5 40.5 L 126.5 44.0 L 126.5 47.0 L 128.5 49.0 L 135.0 49.0 L 144.0 48.5 L 152.5 41.5 L 159.0 34.5 L 162.5 29.0 L 163.0 28.0 M 160.5 85.0 L 151.0 82.0 L 151.0 82.0 L 151.0 79.0 L 151.0 74.5 L 151.0 70.5 L 153.5 68.5 L 159.0 68.0 L 165.5 68.0 L 170.0 72.0 L 173.5 78.0 L 175.0 85.0 L 175.0 92.0 L 174.5 98.5 L 172.5 102.5 L 171.5 104.5 L 171.0 104.5 L 171.5 99.0 L 178.5 88.5 L 187.0 78.0 L 193.0 72.5 L 196.5 72.0 L 198.0 73.5 L 199.0 79.5 L 199.0 87.0 L 199.0 95.0 L 199.0 102.0 L 199.0 108.0 M 216.0 121.5 L 219.5 119.5 L 219.5 119.5 L 225.0 114.0 L 230.5 105.5 L 233.5 94.5 L 234.0 84.0 L 232.0 75.5 L 225.5 71.0 L 220.5 69.0 L 218.5 68.0 L 218.0 67.0 L 224.5 67.0 L 229.5 67.0 M 255.0 79.5 L 255.0 75.0 L 255.0 75.0 L 262.0 75.0 L 270.5 75.5 L 277.5 78.5 L 282.0 82.5 L 284.0 87.5 L 284.0 92.5 L 281.5 99.0 L 276.5 103.5 L 272.0 105.5 L 268.5 106.0 L 266.0 106.0 L 264.5 104.5 L 264.5 102.0 L 265.0 100.0 M 314.0 94.0 L 312.5 87.0 L 312.5 87.0 L 308.5 86.5 L 306.0 84.0 L 304.5 81.5 L 304.5 80.0 L 304.5 79.5 L 309.0 79.5 L 314.5 83.5 L 318.0 90.0 L 320.5 95.5 L 320.5 100.5 L 318.5 103.5 L 313.0 104.0 L 308.0 103.0 L 304.0 96.0 L 303.5 86.5 L 304.5 76.5 L 310.0 70.5 L 316.5 69.0 L 323.5 70.0 L 330.5 75.5 L 336.0 82.5 L 338.5 88.5 L 339.0 94.0 L 339.0 97.5 L 335.0 97.5 L 331.0 94.5 L 327.5 85.0 L 324.0 72.5 L 320.5 57.5 L 315.5 44.0 L 310.5 37.5 L 308.0 36.5 L 308.0 37.0 L 308.5 42.5 L 315.0 48.5 L 323.0 52.5 L 333.5 53.5 L 344.5 53.0 L 354.5 44.5 L 361.5 34.5 L 364.5 23.0 L 365.5 15.0 L 365.5 11.5 L 364.5 10.5 L 363.0 12.0 M 334.0 97.5 L 335.0 97.5 L 335.0 97.5 L 335.0 97.5 L 335.0 97.0 L 335.0 97.0 L 334.5 97.0 L 334.5 97.0 L 334.0 96.5 L 334.0 96.5 L 333.5 96.0 L 333.5 96.0 L 332.5 96.0 L 332.5 96.0 L 332.5 95.5 L 332.5 95.5 L 331.0 95.5 L 331.0 95.5 L 330.5 95.0 L 330.5 95.0 L 329.0 95.0 L 329.0 95.0 L 328.5 94.5 L 328.5 94.5 L 327.0 94.5 L 327.0 94.5 L 325.0 94.0 L 325.0 94.0 L 323.0 94.0 L 323.0 94.0 L 322.5 94.0 L 322.5 94.0 L 320.5 94.0 L 320.5 94.0 L 319.5 94.0 L 319.5 94.0 L 317.0 94.5 L 317.0 94.5 L 316.0 94.5 L 316.0 94.5 L 312.5 95.5 L 312.5 95.5 L 311.5 95.5 L 311.5 95.5 L 309.5 96.0 L 309.5 96.0 L 307.5 96.5 L 307.5 96.5 L 306.5 96.5 L 306.5 96.5 L 304.5 97.0 L 304.5 97.0 L 303.0 97.5 L 303.0 97.5 L 303.0 98.0 L 303.0 98.0 L 304.0 98.0 L 304.0 98.0 L 308.0 97.0 L 308.0 97.0 L 314.5 94.5 L 314.5 94.5 L 321.0 92.0 L 321.0 92.0 L 329.5 88.5 L 329.5 88.5 L 335.5 86.0 L 335.5 86.0 L 338.5 85.0 L 338.5 85.0 L 340.0 84.5 L 340.0 84.5 L 340.5 84.0 L 340.5 84.0 L 340.5 84.0 L 340.5 84.0 L 340.0 84.0 L 340.0 84.0 L 339.5 84.0 L 339.5 84.0 L 338.5 85.0 L 338.5 85.0 L 338.0 86.0 L 338.0 86.0 L 338.0 86.5 L 338.0 86.5 M 25.0 107.5 L 26.5 107.0 L 32.5 104.0 L 32.5 104.0 L 44.5 102.5 L 44.5 102.5 L 64.0 102.0 L 64.0 102.0 L 87.5 100.0 L 87.5 100.0 L 117.5 97.5 L 117.5 97.5 L 152.0 94.0 L 152.0 94.0 L 192.5 90.5 L 192.5 90.5 L 235.5 88.5 L 235.5 88.5 L 279.0 86.5 L 279.0 86.5 L 321.0 84.5 L 321.0 84.5 L 360.5 83.5 L 360.5 83.5 L 394.5 84.0 L 394.5 84.0 L 424.0 86.0 L 424.0 86.0 L 447.0 89.5 L 447.0 89.5 L 462.0 93.5 L 462.0 93.5 L 473.0 97.5 L 473.0 97.5 L 476.0 101.5 L 476.0 101.5 M 86.5 58.5 L 84.0 58.5 L 80.0 58.0 L 80.0 58.0 L 76.5 60.0 L 76.5 60.0 L 70.5 63.5 L 70.5 63.5 L 63.0 68.0 L 63.0 68.0 L 57.0 73.5 L 57.0 73.5 L 57.0 75.5 L 57.0 75.5 L 67.5 69.0 L 67.5 69.0 L 89.0 57.0 L 89.0 57.0 L 119.5 40.0 L 119.5 40.0 L 146.0 28.0 L 146.0 28.0 L 157.5 25.0 L 157.5 25.0 L 154.0 32.0 L 154.0 32.0 L 135.0 47.0 L 135.0 47.0 L 106.0 68.0 L 106.0 68.0 L 77.5 87.5 L 77.5 87.5 L 62.0 96.5 L 62.0 96.5 L 63.0 97.0 L 63.0 97.0 L 79.5 89.5 L 79.5 89.5 L 105.5 77.5 L 105.5 77.5 L 138.0 63.0 L 138.0 63.0 L 161.5 58.0 L 161.5 58.0 L 160.5 63.5 L 160.5 63.5 L 138.0 78.0 L 138.0 78.0 L 103.5 98.5 L 103.5 98.5 L 77.5 113.5 L 77.5 113.5 L 69.0 119.0 L 69.0 119.0 L 76.0 113.5 L 76.0 113.5 L 99.5 99.0 L 99.5 99.0 L 138.0 78.5 L 138.0 78.5 L 180.0 62.5 L 180.0 62.5 L 197.0 59.5 L 197.0 59.5 L 192.5 64.5 L 192.5 64.5 L 170.0 77.5 L 170.0 77.5 L 141.0 92.5 L 141.0 92.5 L 115.0 107.0 L 115.0 107.0 L 106.5 110.5 L 106.5 110.5 L 120.0 104.0 L 120.0 104.0 L 150.0 86.5 L 150.0 86.5 L 192.0 65.5 L 192.0 65.5 L 221.0 54.0 L 221.0 54.0 L 227.5 53.5 L 227.5 53.5 L 210.0 62.5 L 210.0 62.5 L 179.0 77.5 L 179.0 77.5 L 144.5 95.0 L 144.5 95.0 L 124.5 105.0 L 124.5 105.0 L 127.0 104.0 L 127.0 104.0 L 147.5 92.0 L 147.5 92.0 L 182.0 73.0 L 182.0 73.0 L 218.0 56.5 L 218.0 56.5 L 238.5 49.0 L 238.5 49.0 L 238.0 51.0 L 238.0 51.0 L 222.0 60.0 L 222.0 60.0 L 196.5 72.0 L 196.5 72.0 L 172.0 85.0 L 172.0 85.0 L 163.5 89.5 L 163.5 89.5 L 170.0 87.0 L 170.0 87.0 L 196.0 74.5 L 196.0 74.5 L 232.0 58.5 L 232.0 58.5 L 265.5 45.0 L 265.5 45.0 L 280.0 43.0 L 280.0 43.0 L 272.5 48.0 L 272.5 48.0 L 248.0 61.5 L 248.0 61.5 L 216.0 78.5 L 216.0 78.5 L 191.0 92.0 L 191.0 92.0 L 182.0 100.0 L 182.0 100.0 L 186.5 99.5 L 186.5 99.5 L 205.5 89.5 L 205.5 89.5 L 237.0 75.0 L 237.0 75.0 L 278.0 58.0 L 278.0 58.0 L 307.5 47.5 L 307.5 47.5 L 319.0 46.0 L 319.0 46.0 L 309.0 53.5 L 309.0 53.5 L 286.5 65.5 L 286.5 65.5 L 261.0 80.0 L 261.0 80.0 L 244.0 93.0 L 244.0 93.0 L 239.5 103.5 L 239.5 103.5 L 242.0 107.0 L 242.0 107.0' stroke='#111' stroke-width='2' fill='none' stroke-linejoin='round' stroke-linecap='round'/></svg>	2026-07-16 16:22:05.75121+07	approved	2026-07-16 16:26:40.877647+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	นางสาว
\.


--
-- Data for Name: ta_profiles; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.ta_profiles (user_id, national_id, bank_name, bank_branch, branch_code, account_no, account_name, signature_svg, completed_at, verified_at, verified_by, reject_reason, status, signature_png_b64, current_round, prefix) FROM stdin;
7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	\N	pending	\N	1	\N
8d7211e1-8a6e-469b-b9ac-653df81f83ed	\N	ธนาคารกสิกรไทย	นครราชสีมา	4567	2131515456	นางสาวภัทรวดี วงศ์นอก	<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 140'><path d='M 62.5 105.0 M 105.0 104.5 L 99.5 96.0 L 99.5 96.0 L 97.5 95.0 L 94.5 94.5 L 91.0 95.5 L 88.5 101.5 L 88.0 107.0 L 88.0 112.0 L 90.5 114.5 L 95.0 114.5 L 101.5 114.5 L 107.0 108.5 L 112.0 101.0 L 116.0 93.5 L 117.0 87.5 L 117.0 84.5 L 112.0 82.0 L 107.5 80.5 L 105.0 78.5 L 102.5 77.0 L 102.5 76.0 L 102.5 75.5 L 105.0 74.5 L 111.5 73.5 L 119.0 73.0 L 126.0 73.0 L 131.5 73.0 L 134.5 77.5 L 135.5 83.5 L 135.5 90.0 L 134.0 96.0 L 132.0 101.0 L 131.5 104.5 L 131.5 107.0 L 131.5 109.5 M 134.0 55.0 L 130.5 51.5 L 130.5 51.5 L 131.5 45.5 L 131.5 41.0 L 131.5 39.0 L 130.0 38.5 L 127.5 38.5 L 126.5 40.5 L 126.5 44.0 L 126.5 47.0 L 128.5 49.0 L 135.0 49.0 L 144.0 48.5 L 152.5 41.5 L 159.0 34.5 L 162.5 29.0 L 163.0 28.0 M 160.5 85.0 L 151.0 82.0 L 151.0 82.0 L 151.0 79.0 L 151.0 74.5 L 151.0 70.5 L 153.5 68.5 L 159.0 68.0 L 165.5 68.0 L 170.0 72.0 L 173.5 78.0 L 175.0 85.0 L 175.0 92.0 L 174.5 98.5 L 172.5 102.5 L 171.5 104.5 L 171.0 104.5 L 171.5 99.0 L 178.5 88.5 L 187.0 78.0 L 193.0 72.5 L 196.5 72.0 L 198.0 73.5 L 199.0 79.5 L 199.0 87.0 L 199.0 95.0 L 199.0 102.0 L 199.0 108.0 M 216.0 121.5 L 219.5 119.5 L 219.5 119.5 L 225.0 114.0 L 230.5 105.5 L 233.5 94.5 L 234.0 84.0 L 232.0 75.5 L 225.5 71.0 L 220.5 69.0 L 218.5 68.0 L 218.0 67.0 L 224.5 67.0 L 229.5 67.0 M 255.0 79.5 L 255.0 75.0 L 255.0 75.0 L 262.0 75.0 L 270.5 75.5 L 277.5 78.5 L 282.0 82.5 L 284.0 87.5 L 284.0 92.5 L 281.5 99.0 L 276.5 103.5 L 272.0 105.5 L 268.5 106.0 L 266.0 106.0 L 264.5 104.5 L 264.5 102.0 L 265.0 100.0 M 314.0 94.0 L 312.5 87.0 L 312.5 87.0 L 308.5 86.5 L 306.0 84.0 L 304.5 81.5 L 304.5 80.0 L 304.5 79.5 L 309.0 79.5 L 314.5 83.5 L 318.0 90.0 L 320.5 95.5 L 320.5 100.5 L 318.5 103.5 L 313.0 104.0 L 308.0 103.0 L 304.0 96.0 L 303.5 86.5 L 304.5 76.5 L 310.0 70.5 L 316.5 69.0 L 323.5 70.0 L 330.5 75.5 L 336.0 82.5 L 338.5 88.5 L 339.0 94.0 L 339.0 97.5 L 335.0 97.5 L 331.0 94.5 L 327.5 85.0 L 324.0 72.5 L 320.5 57.5 L 315.5 44.0 L 310.5 37.5 L 308.0 36.5 L 308.0 37.0 L 308.5 42.5 L 315.0 48.5 L 323.0 52.5 L 333.5 53.5 L 344.5 53.0 L 354.5 44.5 L 361.5 34.5 L 364.5 23.0 L 365.5 15.0 L 365.5 11.5 L 364.5 10.5 L 363.0 12.0 M 334.0 97.5 L 335.0 97.5 L 335.0 97.5 L 335.0 97.5 L 335.0 97.0 L 335.0 97.0 L 334.5 97.0 L 334.5 97.0 L 334.0 96.5 L 334.0 96.5 L 333.5 96.0 L 333.5 96.0 L 332.5 96.0 L 332.5 96.0 L 332.5 95.5 L 332.5 95.5 L 331.0 95.5 L 331.0 95.5 L 330.5 95.0 L 330.5 95.0 L 329.0 95.0 L 329.0 95.0 L 328.5 94.5 L 328.5 94.5 L 327.0 94.5 L 327.0 94.5 L 325.0 94.0 L 325.0 94.0 L 323.0 94.0 L 323.0 94.0 L 322.5 94.0 L 322.5 94.0 L 320.5 94.0 L 320.5 94.0 L 319.5 94.0 L 319.5 94.0 L 317.0 94.5 L 317.0 94.5 L 316.0 94.5 L 316.0 94.5 L 312.5 95.5 L 312.5 95.5 L 311.5 95.5 L 311.5 95.5 L 309.5 96.0 L 309.5 96.0 L 307.5 96.5 L 307.5 96.5 L 306.5 96.5 L 306.5 96.5 L 304.5 97.0 L 304.5 97.0 L 303.0 97.5 L 303.0 97.5 L 303.0 98.0 L 303.0 98.0 L 304.0 98.0 L 304.0 98.0 L 308.0 97.0 L 308.0 97.0 L 314.5 94.5 L 314.5 94.5 L 321.0 92.0 L 321.0 92.0 L 329.5 88.5 L 329.5 88.5 L 335.5 86.0 L 335.5 86.0 L 338.5 85.0 L 338.5 85.0 L 340.0 84.5 L 340.0 84.5 L 340.5 84.0 L 340.5 84.0 L 340.5 84.0 L 340.5 84.0 L 340.0 84.0 L 340.0 84.0 L 339.5 84.0 L 339.5 84.0 L 338.5 85.0 L 338.5 85.0 L 338.0 86.0 L 338.0 86.0 L 338.0 86.5 L 338.0 86.5 M 25.0 107.5 L 26.5 107.0 L 32.5 104.0 L 32.5 104.0 L 44.5 102.5 L 44.5 102.5 L 64.0 102.0 L 64.0 102.0 L 87.5 100.0 L 87.5 100.0 L 117.5 97.5 L 117.5 97.5 L 152.0 94.0 L 152.0 94.0 L 192.5 90.5 L 192.5 90.5 L 235.5 88.5 L 235.5 88.5 L 279.0 86.5 L 279.0 86.5 L 321.0 84.5 L 321.0 84.5 L 360.5 83.5 L 360.5 83.5 L 394.5 84.0 L 394.5 84.0 L 424.0 86.0 L 424.0 86.0 L 447.0 89.5 L 447.0 89.5 L 462.0 93.5 L 462.0 93.5 L 473.0 97.5 L 473.0 97.5 L 476.0 101.5 L 476.0 101.5 M 86.5 58.5 L 84.0 58.5 L 80.0 58.0 L 80.0 58.0 L 76.5 60.0 L 76.5 60.0 L 70.5 63.5 L 70.5 63.5 L 63.0 68.0 L 63.0 68.0 L 57.0 73.5 L 57.0 73.5 L 57.0 75.5 L 57.0 75.5 L 67.5 69.0 L 67.5 69.0 L 89.0 57.0 L 89.0 57.0 L 119.5 40.0 L 119.5 40.0 L 146.0 28.0 L 146.0 28.0 L 157.5 25.0 L 157.5 25.0 L 154.0 32.0 L 154.0 32.0 L 135.0 47.0 L 135.0 47.0 L 106.0 68.0 L 106.0 68.0 L 77.5 87.5 L 77.5 87.5 L 62.0 96.5 L 62.0 96.5 L 63.0 97.0 L 63.0 97.0 L 79.5 89.5 L 79.5 89.5 L 105.5 77.5 L 105.5 77.5 L 138.0 63.0 L 138.0 63.0 L 161.5 58.0 L 161.5 58.0 L 160.5 63.5 L 160.5 63.5 L 138.0 78.0 L 138.0 78.0 L 103.5 98.5 L 103.5 98.5 L 77.5 113.5 L 77.5 113.5 L 69.0 119.0 L 69.0 119.0 L 76.0 113.5 L 76.0 113.5 L 99.5 99.0 L 99.5 99.0 L 138.0 78.5 L 138.0 78.5 L 180.0 62.5 L 180.0 62.5 L 197.0 59.5 L 197.0 59.5 L 192.5 64.5 L 192.5 64.5 L 170.0 77.5 L 170.0 77.5 L 141.0 92.5 L 141.0 92.5 L 115.0 107.0 L 115.0 107.0 L 106.5 110.5 L 106.5 110.5 L 120.0 104.0 L 120.0 104.0 L 150.0 86.5 L 150.0 86.5 L 192.0 65.5 L 192.0 65.5 L 221.0 54.0 L 221.0 54.0 L 227.5 53.5 L 227.5 53.5 L 210.0 62.5 L 210.0 62.5 L 179.0 77.5 L 179.0 77.5 L 144.5 95.0 L 144.5 95.0 L 124.5 105.0 L 124.5 105.0 L 127.0 104.0 L 127.0 104.0 L 147.5 92.0 L 147.5 92.0 L 182.0 73.0 L 182.0 73.0 L 218.0 56.5 L 218.0 56.5 L 238.5 49.0 L 238.5 49.0 L 238.0 51.0 L 238.0 51.0 L 222.0 60.0 L 222.0 60.0 L 196.5 72.0 L 196.5 72.0 L 172.0 85.0 L 172.0 85.0 L 163.5 89.5 L 163.5 89.5 L 170.0 87.0 L 170.0 87.0 L 196.0 74.5 L 196.0 74.5 L 232.0 58.5 L 232.0 58.5 L 265.5 45.0 L 265.5 45.0 L 280.0 43.0 L 280.0 43.0 L 272.5 48.0 L 272.5 48.0 L 248.0 61.5 L 248.0 61.5 L 216.0 78.5 L 216.0 78.5 L 191.0 92.0 L 191.0 92.0 L 182.0 100.0 L 182.0 100.0 L 186.5 99.5 L 186.5 99.5 L 205.5 89.5 L 205.5 89.5 L 237.0 75.0 L 237.0 75.0 L 278.0 58.0 L 278.0 58.0 L 307.5 47.5 L 307.5 47.5 L 319.0 46.0 L 319.0 46.0 L 309.0 53.5 L 309.0 53.5 L 286.5 65.5 L 286.5 65.5 L 261.0 80.0 L 261.0 80.0 L 244.0 93.0 L 244.0 93.0 L 239.5 103.5 L 239.5 103.5 L 242.0 107.0 L 242.0 107.0' stroke='#111' stroke-width='2' fill='none' stroke-linejoin='round' stroke-linecap='round'/></svg>	2026-07-16 16:22:05.75121+07	2026-07-16 16:26:40.877647+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	approved	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	1	นางสาว
b134e943-7410-44fd-883b-0b32f4a93b33	\N	ธนาคารไทยพาณิชย์	เทสโก้ โลตัส หนองบัวลำภู	5311	4091290303	นายสุพพิธาน ภักสวัสดิ์	<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 140'><path d='M 161.0 94.4 L 161.0 93.4 L 162.0 92.4 L 166.0 88.4 L 179.0 73.4 L 184.0 70.4 L 192.0 68.4 L 201.0 67.4 L 209.0 67.4 L 219.0 70.4 L 220.0 71.4 L 222.0 73.4 L 223.0 74.4 L 225.0 77.4 L 226.0 79.4 L 231.0 84.4 L 232.0 85.4 L 233.0 85.4 L 233.0 82.4 L 233.0 79.4 L 231.0 76.4 L 230.0 72.4 L 219.0 50.4 L 216.0 45.4 L 211.0 39.4 L 208.0 36.4 L 207.0 35.4 L 203.0 34.4 L 196.0 34.4 L 184.0 39.4 L 171.0 46.4 L 164.0 54.4 L 162.0 56.4 L 159.0 62.4 L 157.0 70.4 L 156.0 74.4 L 156.0 76.4 L 156.0 77.4 M 181.0 77.4 L 182.0 76.4 L 187.0 71.4 L 197.0 61.4 L 224.0 39.4 L 239.0 30.4 L 245.0 27.4 L 248.0 24.4' stroke='#111' stroke-width='2' fill='none' stroke-linejoin='round' stroke-linecap='round'/></svg>	2026-07-09 09:21:30.751553+07	2026-07-09 10:06:49.322967+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	approved	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	1	นาย
afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	\N	ธนาคารไทยพาณิชย์	ขอนแก่น	0555	1234567890	นายวรพจน์ สุวรรณภิภพ	<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 140'><path d='M 256.0 95.8 L 255.0 95.8 L 254.0 95.8 L 251.0 95.8 L 250.0 95.8 L 250.0 96.8 L 250.0 99.8 L 250.0 108.8 L 251.0 126.8 L 252.0 128.8 L 255.0 130.8 L 260.0 131.8 L 265.0 131.8 L 281.0 127.8 L 281.0 126.8 L 281.0 122.8 L 282.0 116.8 L 282.0 92.8 L 282.0 77.8 L 282.0 73.8 L 281.0 69.8 L 274.0 57.8 L 271.0 53.8 L 266.0 47.8 L 261.0 45.8 L 249.0 44.8 L 240.0 47.8 L 216.0 60.8 L 209.0 65.8 L 196.0 78.8' stroke='#111' stroke-width='2' fill='none' stroke-linejoin='round' stroke-linecap='round'/></svg>	2026-07-09 19:14:25.678753+07	2026-07-09 19:16:35.921298+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	approved	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	1	นาย
1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	\N	ธนาคารทหารไทยธนชาต	ขอนแก่น	6789	1234567890	นายธนเดช วาตรีบุญเรือง	<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 140'><path d='M 205.0 74.8 L 204.0 73.8 L 198.0 65.8 L 195.0 60.8 L 189.0 52.8 L 188.0 50.8 L 189.0 49.8 L 206.0 58.8 L 212.0 62.8 L 227.0 77.8 L 233.0 88.8 L 238.0 98.8 L 240.0 103.8 L 240.0 104.8 L 241.0 103.8 L 241.0 97.8 L 240.0 84.8 L 235.0 64.8 L 233.0 55.8 L 228.0 39.8 L 227.0 37.8 L 226.0 37.8 L 225.0 37.8 L 215.0 37.8 L 205.0 38.8 L 175.0 44.8 L 168.0 47.8 L 167.0 48.8 L 167.0 49.8' stroke='#111' stroke-width='2' fill='none' stroke-linejoin='round' stroke-linecap='round'/></svg>	2026-07-14 02:38:24.726099+07	\N	\N	\N	submitted	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	1	นาย
67959d3a-87dc-476f-ab3e-0ce6c054a444	\N	ธนาคารไทยพาณิชย์	ขอนแก่น	1234	1234567890	นางสาวจุฑามาศ  ชะรานันท์	<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 500 140'><path d='M 229.0 78.8 L 227.0 78.8 L 224.0 78.8 L 221.0 78.8 L 217.0 77.8 L 214.0 75.8 L 213.0 74.8 L 212.0 72.8 L 212.0 71.8 L 213.0 71.8 L 214.0 71.8 L 217.0 71.8 L 220.0 71.8 L 224.0 71.8 L 228.0 71.8 L 232.0 71.8 L 237.0 72.8 L 238.0 73.8 L 238.0 74.8 L 239.0 76.8 L 240.0 79.8 L 245.0 88.8 L 247.0 93.8 L 249.0 97.8 L 251.0 101.8 L 252.0 104.8 L 253.0 104.8 L 253.0 105.8 L 253.0 104.8 L 253.0 103.8 L 253.0 102.8 L 253.0 101.8 L 252.0 97.8 L 251.0 92.8 L 249.0 85.8 L 247.0 77.8 L 245.0 68.8 L 244.0 59.8 L 242.0 53.8 L 241.0 48.8 L 240.0 45.8 L 239.0 44.8 L 238.0 44.8 L 237.0 44.8 L 236.0 44.8 L 233.0 45.8 L 225.0 50.8 L 213.0 57.8 L 201.0 65.8 L 191.0 72.8 L 185.0 77.8 L 180.0 82.8 L 176.0 87.8' stroke='#111' stroke-width='2' fill='none' stroke-linejoin='round' stroke-linecap='round'/></svg>	2026-07-14 02:15:04.694293+07	2026-07-17 10:47:45.029768+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	approved	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	1	นางสาว
\.


--
-- Data for Name: ta_request_assignments; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.ta_request_assignments (id, request_id, section_id, ta_id, level) FROM stdin;
273f3713-22b5-4b28-b281-069701f6a462	a83c369d-00d1-45d2-b9cf-cdada2eb8f96	13a54f98-c08c-453e-adcc-dc9971b07ba1	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	phd
11fe398c-d73f-484e-b804-abf39d00998f	a83c369d-00d1-45d2-b9cf-cdada2eb8f96	13a54f98-c08c-453e-adcc-dc9971b07ba1	b134e943-7410-44fd-883b-0b32f4a93b33	undergrad
0f65bf19-f922-42b1-a165-04710b7b872a	5c01f193-e90e-44e4-ba3f-070a9f5fd75a	aeb5e366-7da6-4286-a823-c878c1c903fb	b134e943-7410-44fd-883b-0b32f4a93b33	undergrad
8b7de86a-d361-4077-860b-b8b02d1dfca4	5c01f193-e90e-44e4-ba3f-070a9f5fd75a	db603f69-2872-48a1-8d7f-a9dafebbd0dd	b134e943-7410-44fd-883b-0b32f4a93b33	undergrad
fc6b5934-e360-45f1-97b6-7b58d1e5257e	5c01f193-e90e-44e4-ba3f-070a9f5fd75a	aeb5e366-7da6-4286-a823-c878c1c903fb	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	phd
dfffcd50-5cc0-44f2-b955-562ca1428c67	5c01f193-e90e-44e4-ba3f-070a9f5fd75a	db603f69-2872-48a1-8d7f-a9dafebbd0dd	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	phd
dca381b6-81c1-4ff9-bf94-683f0002c382	5c01f193-e90e-44e4-ba3f-070a9f5fd75a	aeb5e366-7da6-4286-a823-c878c1c903fb	67959d3a-87dc-476f-ab3e-0ce6c054a444	undergrad
b0f67c9b-e544-4219-a123-18e7aefeaf1c	5c01f193-e90e-44e4-ba3f-070a9f5fd75a	db603f69-2872-48a1-8d7f-a9dafebbd0dd	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	undergrad
f2238f47-c954-48d3-b7d6-6b88c60f33d5	73c55388-57e7-4def-b204-e588b8c28a54	aeb5e366-7da6-4286-a823-c878c1c903fb	67959d3a-87dc-476f-ab3e-0ce6c054a444	undergrad
2adbe69b-0604-4d3c-abb3-12ad5d4dfbbe	73c55388-57e7-4def-b204-e588b8c28a54	db603f69-2872-48a1-8d7f-a9dafebbd0dd	67959d3a-87dc-476f-ab3e-0ce6c054a444	undergrad
10b8130f-dea7-402b-863c-8d0816af981a	73c55388-57e7-4def-b204-e588b8c28a54	db603f69-2872-48a1-8d7f-a9dafebbd0dd	1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	undergrad
1d1c2e0c-c8d4-454b-8bea-5313c055617e	73c55388-57e7-4def-b204-e588b8c28a54	aeb5e366-7da6-4286-a823-c878c1c903fb	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	phd
f82494cc-f45c-4b22-9fe8-57d6f9bc8313	73c55388-57e7-4def-b204-e588b8c28a54	db603f69-2872-48a1-8d7f-a9dafebbd0dd	afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	phd
17cf718e-223d-447e-8974-437a5ef678de	73c55388-57e7-4def-b204-e588b8c28a54	db603f69-2872-48a1-8d7f-a9dafebbd0dd	b134e943-7410-44fd-883b-0b32f4a93b33	undergrad
96f9b8d4-abc0-4372-807f-f0617bf610e7	78226706-5d19-4c03-9369-359d1f814777	47c9d7aa-35f5-4f4c-a833-2d8ea3593c48	b134e943-7410-44fd-883b-0b32f4a93b33	undergrad
cec7a3e6-e513-4520-a889-bdda288bd552	27bf13f1-a400-4122-9f81-1217f525e789	ec11dd51-2af3-40da-afa9-8703523b837e	8d7211e1-8a6e-469b-b9ac-653df81f83ed	undergrad
8b3c02f2-b6a8-4812-bec8-82a360920ac7	27bf13f1-a400-4122-9f81-1217f525e789	80129309-def2-47ee-a51a-423fdcbd0808	8d7211e1-8a6e-469b-b9ac-653df81f83ed	undergrad
b7699829-6b62-4b11-89e0-78d605f24d99	27bf13f1-a400-4122-9f81-1217f525e789	130f437e-714e-4959-923d-266135e0ae42	8d7211e1-8a6e-469b-b9ac-653df81f83ed	undergrad
69048d9c-db89-4fc0-9f18-b2f84f04ffaa	5bcaa54a-781e-448f-af38-73b1e8ba94f5	ec11dd51-2af3-40da-afa9-8703523b837e	8d7211e1-8a6e-469b-b9ac-653df81f83ed	undergrad
29d9ec72-8817-436d-871c-3fa533eb3173	662320f6-224e-4cea-acbe-2c34a82bdb23	ec11dd51-2af3-40da-afa9-8703523b837e	2e25e60a-6743-4fe9-bc89-ae5c9c733730	undergrad
d0d8a204-b8c1-41ec-aed8-696bdf1f4ef6	662320f6-224e-4cea-acbe-2c34a82bdb23	130f437e-714e-4959-923d-266135e0ae42	2e25e60a-6743-4fe9-bc89-ae5c9c733730	undergrad
8bf88447-8544-4e9f-8e12-1a2fcba612ad	662320f6-224e-4cea-acbe-2c34a82bdb23	80129309-def2-47ee-a51a-423fdcbd0808	2e25e60a-6743-4fe9-bc89-ae5c9c733730	undergrad
\.


--
-- Data for Name: ta_request_counts; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.ta_request_counts (request_id, section_id, undergrad_count, graduate_count) FROM stdin;
a83c369d-00d1-45d2-b9cf-cdada2eb8f96	13a54f98-c08c-453e-adcc-dc9971b07ba1	1	1
a83c369d-00d1-45d2-b9cf-cdada2eb8f96	95f12def-b084-4e87-8532-530a6deeef70	0	0
5c01f193-e90e-44e4-ba3f-070a9f5fd75a	aeb5e366-7da6-4286-a823-c878c1c903fb	2	1
5c01f193-e90e-44e4-ba3f-070a9f5fd75a	db603f69-2872-48a1-8d7f-a9dafebbd0dd	2	1
73c55388-57e7-4def-b204-e588b8c28a54	aeb5e366-7da6-4286-a823-c878c1c903fb	1	1
73c55388-57e7-4def-b204-e588b8c28a54	db603f69-2872-48a1-8d7f-a9dafebbd0dd	3	1
78226706-5d19-4c03-9369-359d1f814777	47c9d7aa-35f5-4f4c-a833-2d8ea3593c48	1	0
27bf13f1-a400-4122-9f81-1217f525e789	ec11dd51-2af3-40da-afa9-8703523b837e	1	0
27bf13f1-a400-4122-9f81-1217f525e789	80129309-def2-47ee-a51a-423fdcbd0808	1	0
27bf13f1-a400-4122-9f81-1217f525e789	130f437e-714e-4959-923d-266135e0ae42	1	0
5bcaa54a-781e-448f-af38-73b1e8ba94f5	ec11dd51-2af3-40da-afa9-8703523b837e	1	0
5bcaa54a-781e-448f-af38-73b1e8ba94f5	80129309-def2-47ee-a51a-423fdcbd0808	0	0
5bcaa54a-781e-448f-af38-73b1e8ba94f5	130f437e-714e-4959-923d-266135e0ae42	0	0
662320f6-224e-4cea-acbe-2c34a82bdb23	ec11dd51-2af3-40da-afa9-8703523b837e	1	0
662320f6-224e-4cea-acbe-2c34a82bdb23	80129309-def2-47ee-a51a-423fdcbd0808	1	0
662320f6-224e-4cea-acbe-2c34a82bdb23	130f437e-714e-4959-923d-266135e0ae42	1	0
\.


--
-- Data for Name: ta_request_windows; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.ta_request_windows (id, term_id, opens_at, closes_at, is_open, note) FROM stdin;
dca519a3-484b-4ab3-b9d0-ae9876f14f69	2a01f439-a013-4f5f-a819-5ef591497243	2026-07-09 22:16:00+07	2026-08-08 22:16:00+07	t	\N
\.


--
-- Data for Name: ta_requests; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.ta_requests (id, teaching_course_id, window_id, lecturer_id, reimburse_scope, status, submitted_at, decided_at, decided_by, reject_reason, created_at, updated_at, decision_checks, is_late) FROM stdin;
a83c369d-00d1-45d2-b9cf-cdada2eb8f96	4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	\N	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	both	approved	2026-07-09 22:16:45.898734+07	2026-07-09 22:28:16.628556+07	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	\N	2026-07-09 22:16:45.898734+07	2026-07-09 22:28:16.628556+07	[]	f
5c01f193-e90e-44e4-ba3f-070a9f5fd75a	1c0b6bb3-87ec-43ff-9b63-ff0a491ad4d7	dca519a3-484b-4ab3-b9d0-ae9876f14f69	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	both	rejected	2026-07-14 05:32:14.373199+07	2026-07-14 05:32:14.373199+07	\N	ธนเดช วาตรีบุญเรือง ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่ • จุฑามาศ ชะรานันท์ ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่ • เวลาสอนของ section นี้ทับซ้อนกับตารางเรียนของ สุพพิธา…	2026-07-14 05:32:14.373199+07	2026-07-14 05:32:14.373199+07	[{"ta": "ธนเดช วาตรีบุญเรือง", "rule": "docs", "passed": false, "message": "ธนเดช วาตรีบุญเรือง ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "schedule", "passed": true, "message": "ธนเดช วาตรีบุญเรือง บันทึกตารางเรียนแล้ว"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "duplicate", "passed": true, "message": "ธนเดช วาตรีบุญเรือง ไม่ซ้ำในวิชานี้"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "cap", "passed": true, "message": "ธนเดช วาตรีบุญเรือง เป็นผู้ช่วยสอนอยู่ 0 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "workload", "passed": true, "message": "ธนเดช วาตรีบุญเรือง ระบุภาระงาน 6.00 ชม./สัปดาห์"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "docs", "passed": false, "message": "จุฑามาศ ชะรานันท์ ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "schedule", "passed": true, "message": "จุฑามาศ ชะรานันท์ บันทึกตารางเรียนแล้ว"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "duplicate", "passed": true, "message": "จุฑามาศ ชะรานันท์ ไม่ซ้ำในวิชานี้"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "cap", "passed": true, "message": "จุฑามาศ ชะรานันท์ เป็นผู้ช่วยสอนอยู่ 0 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "workload", "passed": true, "message": "จุฑามาศ ชะรานันท์ ระบุภาระงาน 6.00 ชม./สัปดาห์"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "docs", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เอกสารครบและได้รับการอนุมัติ"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "schedule", "passed": true, "message": "วรพจน์ สุวรรณภิภพ บันทึกตารางเรียนของภาคเรียนแล้ว"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "duplicate", "passed": true, "message": "วรพจน์ สุวรรณภิภพ ไม่ซ้ำในวิชานี้"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "cap", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เป็นผู้ช่วยสอนอยู่ 1 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "workload", "passed": true, "message": "วรพจน์ สุวรรณภิภพ ภาระงานรวม 12.00 ชม./สัปดาห์ (อยู่ในช่วง 10–12)"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "docs", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เอกสารครบและได้รับการอนุมัติ"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "schedule", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ บันทึกตารางเรียนของภาคเรียนแล้ว"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "duplicate", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ ไม่ซ้ำในวิชานี้"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "cap", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เป็นผู้ช่วยสอนอยู่ 1 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "workload", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ ระบุภาระงาน 10.00 ชม./สัปดาห์"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "own_conflict", "passed": true, "message": "ธนเดช วาตรีบุญเรือง เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "cross_conflict", "passed": true, "message": "ธนเดช วาตรีบุญเรือง เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "own_conflict", "passed": true, "message": "จุฑามาศ ชะรานันท์ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "cross_conflict", "passed": true, "message": "จุฑามาศ ชะรานันท์ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "own_conflict", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "cross_conflict", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "own_conflict", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "cross_conflict", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "own_conflict", "passed": false, "message": "เวลาสอนของ section นี้ทับซ้อนกับตารางเรียนของ สุพพิธาน ภักสวัสดิ์"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "cross_conflict", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "own_conflict", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "cross_conflict", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "intra_conflict", "passed": true, "message": "วรพจน์ สุวรรณภิภพ section ในคำขอเดียวกันไม่ทับซ้อน"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "intra_conflict", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ section ในคำขอเดียวกันไม่ทับซ้อน"}]	f
73c55388-57e7-4def-b204-e588b8c28a54	1c0b6bb3-87ec-43ff-9b63-ff0a491ad4d7	dca519a3-484b-4ab3-b9d0-ae9876f14f69	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	both	approved	2026-07-14 05:54:14.177197+07	2026-07-14 05:54:14.177197+07	\N	\N	2026-07-14 05:54:14.177197+07	2026-07-14 05:54:14.177197+07	[{"ta": "ธนเดช วาตรีบุญเรือง", "rule": "docs", "passed": false, "message": "ธนเดช วาตรีบุญเรือง ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่", "warning": true}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "schedule", "passed": true, "message": "ธนเดช วาตรีบุญเรือง บันทึกตารางเรียนแล้ว"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "duplicate", "passed": true, "message": "ธนเดช วาตรีบุญเรือง ไม่ซ้ำในวิชานี้"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "cap", "passed": true, "message": "ธนเดช วาตรีบุญเรือง เป็นผู้ช่วยสอนอยู่ 0 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "workload", "passed": true, "message": "ธนเดช วาตรีบุญเรือง ระบุภาระงาน 6.00 ชม./สัปดาห์"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "docs", "passed": false, "message": "จุฑามาศ ชะรานันท์ ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่", "warning": true}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "schedule", "passed": true, "message": "จุฑามาศ ชะรานันท์ บันทึกตารางเรียนแล้ว"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "duplicate", "passed": true, "message": "จุฑามาศ ชะรานันท์ ไม่ซ้ำในวิชานี้"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "cap", "passed": true, "message": "จุฑามาศ ชะรานันท์ เป็นผู้ช่วยสอนอยู่ 0 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "workload", "passed": true, "message": "จุฑามาศ ชะรานันท์ ระบุภาระงาน 10.00 ชม./สัปดาห์"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "docs", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เอกสารครบและได้รับการอนุมัติ"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "schedule", "passed": true, "message": "วรพจน์ สุวรรณภิภพ บันทึกตารางเรียนของภาคเรียนแล้ว"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "duplicate", "passed": true, "message": "วรพจน์ สุวรรณภิภพ ไม่ซ้ำในวิชานี้"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "cap", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เป็นผู้ช่วยสอนอยู่ 1 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "workload", "passed": true, "message": "วรพจน์ สุวรรณภิภพ ภาระงานรวม 12.00 ชม./สัปดาห์ (อยู่ในช่วง 10–12)"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "docs", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เอกสารครบและได้รับการอนุมัติ"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "schedule", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ บันทึกตารางเรียนของภาคเรียนแล้ว"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "duplicate", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ ไม่ซ้ำในวิชานี้"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "cap", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เป็นผู้ช่วยสอนอยู่ 1 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "workload", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ ระบุภาระงาน 6.00 ชม./สัปดาห์"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "own_conflict", "passed": true, "message": "ธนเดช วาตรีบุญเรือง เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "ธนเดช วาตรีบุญเรือง", "rule": "cross_conflict", "passed": true, "message": "ธนเดช วาตรีบุญเรือง เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "own_conflict", "passed": true, "message": "จุฑามาศ ชะรานันท์ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "cross_conflict", "passed": true, "message": "จุฑามาศ ชะรานันท์ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "own_conflict", "passed": true, "message": "จุฑามาศ ชะรานันท์ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "cross_conflict", "passed": true, "message": "จุฑามาศ ชะรานันท์ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "own_conflict", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "cross_conflict", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "own_conflict", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "cross_conflict", "passed": true, "message": "วรพจน์ สุวรรณภิภพ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "own_conflict", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "cross_conflict", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "จุฑามาศ ชะรานันท์", "rule": "intra_conflict", "passed": true, "message": "จุฑามาศ ชะรานันท์ section ในคำขอเดียวกันไม่ทับซ้อน"}, {"ta": "วรพจน์ สุวรรณภิภพ", "rule": "intra_conflict", "passed": true, "message": "วรพจน์ สุวรรณภิภพ section ในคำขอเดียวกันไม่ทับซ้อน"}]	f
78226706-5d19-4c03-9369-359d1f814777	a416b931-2506-45a8-96c7-d06de9246ff7	dca519a3-484b-4ab3-b9d0-ae9876f14f69	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	both	approved	2026-07-14 10:11:09.144239+07	2026-07-14 10:11:09.144239+07	\N	\N	2026-07-14 10:11:09.144239+07	2026-07-14 10:11:09.144239+07	[{"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "docs", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เอกสารครบและได้รับการอนุมัติ"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "schedule", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ บันทึกตารางเรียนของภาคเรียนแล้ว"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "duplicate", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ ไม่ซ้ำในวิชานี้"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "cap", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เป็นผู้ช่วยสอนอยู่ 2 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "workload", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ ระบุภาระงาน 7.00 ชม./สัปดาห์"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "own_conflict", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "สุพพิธาน ภักสวัสดิ์", "rule": "cross_conflict", "passed": true, "message": "สุพพิธาน ภักสวัสดิ์ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}]	f
27bf13f1-a400-4122-9f81-1217f525e789	ad4b1cff-c174-477c-827b-0a6f63e4beb8	dca519a3-484b-4ab3-b9d0-ae9876f14f69	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	both	rejected	2026-07-16 16:24:21.266584+07	2026-07-16 16:24:21.266584+07	\N	ภัทรวดี วงศ์นอก ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้	2026-07-16 16:24:21.266584+07	2026-07-16 16:24:21.266584+07	[{"ta": "ภัทรวดี วงศ์นอก", "rule": "docs", "passed": false, "message": "ภัทรวดี วงศ์นอก ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่", "warning": true}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "schedule", "passed": true, "message": "ภัทรวดี วงศ์นอก บันทึกตารางเรียนของภาคเรียนแล้ว"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "duplicate", "passed": true, "message": "ภัทรวดี วงศ์นอก ไม่ซ้ำในวิชานี้"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "cap", "passed": true, "message": "ภัทรวดี วงศ์นอก เป็นผู้ช่วยสอนอยู่ 0 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "workload", "passed": true, "message": "ภัทรวดี วงศ์นอก ระบุภาระงาน 10.00 ชม./สัปดาห์"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "own_conflict", "passed": true, "message": "ภัทรวดี วงศ์นอก เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "cross_conflict", "passed": true, "message": "ภัทรวดี วงศ์นอก เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "own_conflict", "passed": true, "message": "ภัทรวดี วงศ์นอก เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "cross_conflict", "passed": true, "message": "ภัทรวดี วงศ์นอก เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "own_conflict", "passed": true, "message": "ภัทรวดี วงศ์นอก เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "cross_conflict", "passed": true, "message": "ภัทรวดี วงศ์นอก เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "intra_conflict", "passed": false, "message": "ภัทรวดี วงศ์นอก ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้"}]	f
5bcaa54a-781e-448f-af38-73b1e8ba94f5	ad4b1cff-c174-477c-827b-0a6f63e4beb8	dca519a3-484b-4ab3-b9d0-ae9876f14f69	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	both	approved	2026-07-16 16:25:18.463793+07	2026-07-16 16:25:18.463793+07	\N	\N	2026-07-16 16:25:18.463793+07	2026-07-16 16:25:18.463793+07	[{"ta": "ภัทรวดี วงศ์นอก", "rule": "docs", "passed": false, "message": "ภัทรวดี วงศ์นอก ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่", "warning": true}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "schedule", "passed": true, "message": "ภัทรวดี วงศ์นอก บันทึกตารางเรียนของภาคเรียนแล้ว"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "duplicate", "passed": true, "message": "ภัทรวดี วงศ์นอก ไม่ซ้ำในวิชานี้"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "cap", "passed": true, "message": "ภัทรวดี วงศ์นอก เป็นผู้ช่วยสอนอยู่ 0 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "workload", "passed": true, "message": "ภัทรวดี วงศ์นอก ระบุภาระงาน 3.00 ชม./สัปดาห์"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "own_conflict", "passed": true, "message": "ภัทรวดี วงศ์นอก เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "ภัทรวดี วงศ์นอก", "rule": "cross_conflict", "passed": true, "message": "ภัทรวดี วงศ์นอก เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}]	f
662320f6-224e-4cea-acbe-2c34a82bdb23	ad4b1cff-c174-477c-827b-0a6f63e4beb8	dca519a3-484b-4ab3-b9d0-ae9876f14f69	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	both	rejected	2026-07-16 16:33:10.28449+07	2026-07-16 16:33:10.28449+07	\N	ณัฐภัทร ประชุมวงษ์ ยังไม่ได้บันทึกตารางเรียนของภาคการศึกษานี้ • ณัฐภัทร ประชุมวงษ์ ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้	2026-07-16 16:33:10.28449+07	2026-07-16 16:33:10.28449+07	[{"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "docs", "passed": false, "message": "ณัฐภัทร ประชุมวงษ์ ยังไม่ผ่านการอนุมัติเอกสารจากเจ้าหน้าที่", "warning": true}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "schedule", "passed": false, "message": "ณัฐภัทร ประชุมวงษ์ ยังไม่ได้บันทึกตารางเรียนของภาคการศึกษานี้"}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "duplicate", "passed": true, "message": "ณัฐภัทร ประชุมวงษ์ ไม่ซ้ำในวิชานี้"}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "cap", "passed": true, "message": "ณัฐภัทร ประชุมวงษ์ เป็นผู้ช่วยสอนอยู่ 0 วิชา (ยังไม่เกินขีดจำกัด 3 วิชา)"}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "workload", "passed": true, "message": "ณัฐภัทร ประชุมวงษ์ ระบุภาระงาน 4.00 ชม./สัปดาห์"}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "own_conflict", "passed": true, "message": "ณัฐภัทร ประชุมวงษ์ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "cross_conflict", "passed": true, "message": "ณัฐภัทร ประชุมวงษ์ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "own_conflict", "passed": true, "message": "ณัฐภัทร ประชุมวงษ์ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "cross_conflict", "passed": true, "message": "ณัฐภัทร ประชุมวงษ์ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "own_conflict", "passed": true, "message": "ณัฐภัทร ประชุมวงษ์ เวลาสอนไม่ทับซ้อนกับตารางเรียนของตัวเอง"}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "cross_conflict", "passed": true, "message": "ณัฐภัทร ประชุมวงษ์ เวลาสอนไม่ทับซ้อนกับวิชาอื่นที่เป็นผู้ช่วยสอนอยู่"}, {"ta": "ณัฐภัทร ประชุมวงษ์", "rule": "intra_conflict", "passed": false, "message": "ณัฐภัทร ประชุมวงษ์ ถูกมอบหมายให้ section ที่เวลาสอนทับซ้อนกันเองในคำขอนี้"}]	f
\.


--
-- Data for Name: ta_review_schedules; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.ta_review_schedules (id, assignment_id, day_of_week, start_time, end_time, room, note, created_at) FROM stdin;
e78d5c64-6d01-4058-8a3b-09e6b46ced8c	96f9b8d4-abc0-4372-807f-f0617bf610e7	6	14:00:00	16:00:00	\N	\N	2026-07-15 07:18:59.953593+07
4fbf9295-896e-4478-9dda-35f968c26f10	17cf718e-223d-447e-8974-437a5ef678de	0	14:00:00	16:00:00	\N	\N	2026-07-16 00:40:51.819954+07
a8b7ee91-47c3-4728-bbf8-0e53261d198e	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	6	14:00:00	16:00:00	\N	\N	2026-07-16 16:27:20.676567+07
2daf760c-a810-466b-9d04-7ca2bcbf9e7e	f2238f47-c954-48d3-b7d6-6b88c60f33d5	6	14:00:00	16:00:00	\N	\N	2026-07-23 19:43:42.81538+07
\.


--
-- Data for Name: ta_workload_forms; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.ta_workload_forms (id, assignment_id, help_teach_hrs, help_teach_desc, prep_hrs, prep_desc, grade_hrs, grade_desc, other_hrs, other_desc, check_work_hrs, attendance_hrs, ug_other_hrs, ug_other_desc, lab_hrs) FROM stdin;
9fedcec7-f18d-42c4-aab8-8ea36bf8c975	273f3713-22b5-4b28-b281-069701f6a462	4.00	ช่วยแนะนํา/สอนปฏิบัตินักศึกษาในคาบเรียน	4.00	เตรียมเอกสาร/เนื้อหา	2.00	ตรวจงานตามที่อาจารย์มอบหมาย	2.00	ช่วยเช็คชื่อ	0.00	0.00	0.00		0.00
0412ff04-747a-46aa-a1c1-e2a24417a310	11fe398c-d73f-484e-b804-abf39d00998f	0.00		0.00		0.00		0.00		4.00	2.00	0.00		4.00
78f38642-bc81-4d2c-b838-f5f58335f7bc	0f65bf19-f922-42b1-a165-04710b7b872a	0.00		0.00		0.00		0.00		4.00	2.00	0.00	ตรวจแต่งกาย	4.00
483bc537-a2be-4652-9b2d-62028a5fcb60	fc6b5934-e360-45f1-97b6-7b58d1e5257e	4.00	ช่วยแนะนํา/สอนปฏิบัตินักศึกษาในคาบเรียน	4.00	เตรียมเอกสาร/เนื้อหา	2.00	ตรวจงานตามที่อาจารย์มอบหมาย	2.00	ช่วยเช็คชื่อ	0.00	0.00	0.00		0.00
1aca37ab-3717-4ac1-a6b1-1bbe26269fbc	dca381b6-81c1-4ff9-bf94-683f0002c382	0.00		0.00		0.00		0.00		2.00	2.00	0.00		2.00
118d7f7b-93d7-4764-933d-19367b58ee0f	b0f67c9b-e544-4219-a123-18e7aefeaf1c	0.00		0.00		0.00		0.00		2.00	2.00	0.00		2.00
e5468232-daec-43a8-8cf8-60a347115281	f2238f47-c954-48d3-b7d6-6b88c60f33d5	0.00		0.00		0.00		0.00		4.00	2.00	0.00		4.00
cf24fa10-fbe2-4d7b-bc47-123f02a91842	10b8130f-dea7-402b-863c-8d0816af981a	0.00		0.00		0.00		0.00		2.00	2.00	0.00		2.00
415149b2-31bd-413e-ba05-a5dafb7f4967	1d1c2e0c-c8d4-454b-8bea-5313c055617e	4.00		4.00		2.00		2.00		0.00	0.00	0.00		0.00
fe27e3f0-90fe-4339-b2be-c2fc85ecdc70	17cf718e-223d-447e-8974-437a5ef678de	0.00		0.00		0.00		0.00		2.00	2.00	0.00		2.00
310431a1-2cea-4c09-b5b6-ca96b6fd5986	96f9b8d4-abc0-4372-807f-f0617bf610e7	0.00		0.00		0.00		0.00		2.00	2.00	1.00		2.00
aa841602-bbd3-4b70-ac43-e52062bfe297	cec7a3e6-e513-4520-a889-bdda288bd552	0.00		0.00		0.00		0.00		4.00	2.00	0.00		4.00
57fd482a-a824-484b-b3f3-0ecea614417b	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	0.00		0.00		0.00		0.00		1.00	1.00	0.00		1.00
ae490c70-841c-4714-9a29-f2b2299ed1c7	29d9ec72-8817-436d-871c-3fa533eb3173	0.00		0.00		0.00		0.00		1.00	1.00	1.00		1.00
\.


--
-- Data for Name: teaching_courses; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.teaching_courses (id, term_id, starts_on, ends_on, num_students, created_by, created_at, updated_at, num_students_regular, num_students_special, exported_at, code, name_th, name_en, level, credits, lecture_hrs, lab_hrs, self_hrs, department) FROM stdin;
4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	2a01f439-a013-4f5f-a819-5ef591497243	2026-06-22	2026-11-03	0	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	2026-07-08 08:53:17.425173+07	2026-07-23 18:51:08.752042+07	0	0	2026-07-10 10:24:05.899815+07	CP323204	การพัฒนาโปรแกรมประยุกต์บนเว็บด้วยภาษาจาวา	\N	undergrad	3	2	2	5	\N
1c0b6bb3-87ec-43ff-9b63-ff0a491ad4d7	2a01f439-a013-4f5f-a819-5ef591497243	2026-06-22	2026-10-22	100	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	2026-07-14 04:44:53.635834+07	2026-07-23 19:43:15.949199+07	100	0	\N	CP421025	การวิเคราะห์และออกแบบซอฟต์แวร์	\N	undergrad	3	2	2	5	\N
a416b931-2506-45a8-96c7-d06de9246ff7	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	80	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	2026-07-14 10:05:50.203216+07	2026-07-15 13:10:57.001351+07	80	0	\N	SC363001	การวิเคราะห์และออกแบบระบบ	\N	undergrad	3	2	2	5	\N
cea72167-38ab-43c8-b80c-8a4b964c90c2	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	0	2abd04e1-b22d-43fa-af57-a7830de6a2ac	2026-07-16 06:41:34.301705+07	2026-07-16 06:41:34.301705+07	0	0	\N	CP351203	การเขียนโปรแกรมเว็บและการประยุกต์ใช้	\N	undergrad	3	2	2	5	\N
ad611905-ad80-4d20-8fba-86339488b093	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	0	2abd04e1-b22d-43fa-af57-a7830de6a2ac	2026-07-16 06:47:32.168795+07	2026-07-16 06:47:32.168795+07	0	0	\N	SC361002	การเขียนโปรแกรมเชิงโครงสร้างสำหรับเทคโนโลยีสารสนเทศ	\N	undergrad	3	2	2	5	\N
ad4b1cff-c174-477c-827b-0a6f63e4beb8	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	95	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	2026-07-16 16:22:50.418481+07	2026-07-16 16:59:26.612937+07	85	10	\N	SC362005	การวิเคราะห์และออกแบบฐานข้อมูล	\N	undergrad	3	2	2	5	\N
1a656edf-6a1b-4de1-b441-51e02d3150de	2a01f439-a013-4f5f-a819-5ef591497243	\N	\N	80	19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	2026-07-16 05:48:13.783114+07	2026-07-17 11:27:26.633733+07	80	0	\N	SC362004	การเขียนโปรแกรมประยุกต์บนเว็บ	\N	undergrad	3	2	2	5	\N
\.


--
-- Data for Name: teaching_lecturers; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.teaching_lecturers (teaching_course_id, lecturer_id, is_primary) FROM stdin;
4415242d-ffdb-45ab-b1d2-b95fa9df1cc8	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	t
1c0b6bb3-87ec-43ff-9b63-ff0a491ad4d7	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	t
a416b931-2506-45a8-96c7-d06de9246ff7	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	t
1a656edf-6a1b-4de1-b441-51e02d3150de	2abd04e1-b22d-43fa-af57-a7830de6a2ac	t
cea72167-38ab-43c8-b80c-8a4b964c90c2	2abd04e1-b22d-43fa-af57-a7830de6a2ac	t
ad611905-ad80-4d20-8fba-86339488b093	2abd04e1-b22d-43fa-af57-a7830de6a2ac	t
ad4b1cff-c174-477c-827b-0a6f63e4beb8	9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	t
\.


--
-- Data for Name: user_roles; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.user_roles (user_id, role) FROM stdin;
19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	admin
19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	staff
7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	lecturer
b134e943-7410-44fd-883b-0b32f4a93b33	ta
afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	ta
c82ac668-6004-45f1-bcbb-f762610dadaf	lecturer
9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	lecturer
2abd04e1-b22d-43fa-af57-a7830de6a2ac	lecturer
67959d3a-87dc-476f-ab3e-0ce6c054a444	ta
1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	ta
8d7211e1-8a6e-469b-b9ac-653df81f83ed	ta
2e25e60a-6743-4fe9-bc89-ae5c9c733730	ta
\.


--
-- Data for Name: users; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.users (id, email, first_name, last_name, phone, password_hash, sso_subject, study_level, student_id, department, is_active, profile_completed, created_at, updated_at, deleted_at, title, must_change_password, study_year) FROM stdin;
19ff049b-34d5-4e3e-be1e-1e78ed4e35ac	admin@tapayment.com	Admin	COCO	\N	$2a$10$YGxZCZ5ONsGMy4CaA8WiseT4qm2zeufNUHHyF/rxXnunGfRq3Mlea	\N	\N	\N	\N	t	t	2026-07-08 01:55:56.506844+07	2026-07-08 01:55:56.506844+07	\N	\N	f	\N
7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	jakkritk@kku.ac.th	จักรกฤษณ์	แก้วโยธา		$2a$10$IGtFl.y6hGs/mDNhiEntYuHhTYsJZ01BET4R1APaLoA5YLpxmzNC.	\N	\N	\N	\N	t	f	2026-07-08 07:52:39.464733+07	2026-07-08 07:58:20.253126+07	\N	อ. ดร.	f	\N
2e25e60a-6743-4fe9-bc89-ae5c9c733730	nattapat.pr@kkumail.com	ณัฐภัทร	ประชุมวงษ์	\N	$2a$10$bBFwobONTRqxmf9TmqtsKOxdcslNaXiK2UbdPHlVWchE3QdcIBE6W	\N	undergrad	\N	\N	t	f	2026-07-16 16:15:35.023464+07	2026-07-16 16:15:35.023464+07	\N	นาย	t	4
8d7211e1-8a6e-469b-b9ac-653df81f83ed	evenerak2547@gmail.com	ภัทรวดี	วงศ์นอก	0283534272	$2a$10$WmhM5.b1vMVJ9RHSRumDeOVXfUQZNXCGiOgDxBnx4h4WFCpiWJtUy	\N	undergrad	654321456-4	\N	t	f	2026-07-16 16:14:06.191384+07	2026-07-16 16:22:05.75121+07	\N	นางสาว	f	4
afa52de0-5920-4dd1-ae74-4a5c7f5bd9d3	worapoj_s@kkumail.com	วรพจน์	สุวรรณภิภพ	0987654321	$2a$10$gcrbEsskb1jgZtceref8ueWtSDpWudaWzMaf/C2gVYQC8GfO5k52O	\N	phd	653020123-4	\N	t	f	2026-07-09 19:10:45.10843+07	2026-07-17 10:18:19.664235+07	\N	นาย	t	\N
2abd04e1-b22d-43fa-af57-a7830de6a2ac	waruwu@kku.ac.th	วรัญญา	วรรณศรี	\N	$2a$10$tDyJPuhYmwCwRSxzzM36lecAbF7d47YtAknJEfJWsSIo/qYXF4wYK	\N	\N	\N	\N	t	f	2026-07-12 13:40:21.956977+07	2026-07-16 05:51:36.699811+07	\N	ผศ. ดร.	f	\N
9cca9058-2ec5-4d6c-aa6e-82c30e5a699d	monlwa@kku.ac.th	มัลลิกา	วัฒนะ	\N	$2a$10$rbDIGQoSHgzZloqCIWntVOq2dk8IvXz2g2CZdQHaJBjA6sLsiy7KK	\N	\N	\N	\N	t	f	2026-07-12 13:39:24.148583+07	2026-07-16 16:17:01.688262+07	\N	ผศ. ดร.	f	\N
c82ac668-6004-45f1-bcbb-f762610dadaf	isoonkan@kku.ac.th	ไอศูรย์	กาญจนสุรัตน์	\N	$2a$10$UYLZnqchCzGPCGBAtD0Qxe2as/3Uak6nkYU2qVYole9OnSeGo0Cue	\N	\N	\N	\N	t	f	2026-07-12 13:38:15.163783+07	2026-07-12 13:38:15.163783+07	\N	ผศ. ดร.	t	\N
67959d3a-87dc-476f-ab3e-0ce6c054a444	chuthamat.cha@kkumail.com	จุฑามาศ	ชะรานันท์	0987654321	$2a$10$Joe4gxequh0siMcYMC.YheHULULpdpkzOjffL1NaYMj3fKKIS9V2a	\N	undergrad	234567890-1	\N	t	f	2026-07-14 01:23:12.269674+07	2026-07-23 19:42:25.851722+07	\N	นางสาว	f	\N
1a7a52ee-435e-40f9-b9ff-dad6725ecc7c	thanadet.w@kkumail.com	ธนเดช	วาตรีบุญเรือง	0987654321	$2a$10$bF1i/OIV21TrmO9bt/Hz4O95vWbe448B8j40mrYNln4TLtrC4WO.C	\N	undergrad	456789012-3	\N	t	f	2026-07-14 02:18:21.194935+07	2026-07-14 02:38:24.726099+07	\N	นาย	f	\N
b134e943-7410-44fd-883b-0b32f4a93b33	supphitan.p@kkumail.com	สุพพิธาน	ภักสวัสดิ์	0648801344	$2a$10$1Xork9f5VAxhmJWdCamAu.En04F0I5KP288I1TIlMwA4d3Wp0Tu6i	\N	undergrad	633020334-8	\N	t	f	2026-07-08 10:23:12.12057+07	2026-07-14 05:58:03.592786+07	\N	นาย	f	\N
\.


--
-- Data for Name: work_logs; Type: TABLE DATA; Schema: public; Owner: itii_database_prod
--

COPY public.work_logs (id, assignment_id, work_date, start_time, end_time, hours, activity, room, note, status, submitted_at, approved_at, approved_by, reject_reason, parent_kind) FROM stdin;
28d6388a-d5bb-4003-866c-4cc280ee1ab9	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-06-26	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
ab504de4-54e9-4261-9d53-1e8c906fe971	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-06-26	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
e472f133-bfad-4305-babd-f84486315c0d	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-03	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
d9b37c27-eba3-4f4c-ad79-22bad0bf254f	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-03	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
7ab1d975-74d7-4385-9504-c06dc4e2b62e	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-10	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
bcc4406e-61b3-4c44-bae3-ba77debb660f	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-10	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
f6739f24-0cd3-48ed-9429-bfbd19eed5a7	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-17	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
54e077f7-fcfd-4c29-898a-b69cac9063a6	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-17	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
1b0a43d4-d70b-422f-b9b0-10a12c14dd19	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-24	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
5339d36a-f64e-4be1-92d0-42f8d58e5329	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-24	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
95975d4c-3cbf-4b49-beeb-f250dee08b66	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-31	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
39f74663-5eee-40b5-a650-bd9f417f7b98	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-31	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
97d75cb3-6425-4dd9-8b85-a74bf0acf477	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-07	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
f353da6e-6341-406c-8985-9c0389401aa4	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-07	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
5108ce52-3c8a-41f6-ae48-14e419727596	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-14	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
fe0593d3-892c-4a48-9e17-caec4177a81b	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-14	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
a3ea2135-b765-4f71-ac1e-766f3b7dcc31	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-21	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
52d0810a-641a-426f-9d2a-33d1f948ad04	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-21	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
e8ca5408-6c29-4835-a094-87c335b3d673	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-28	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
5957c26d-b348-4277-8d2f-6b8b204602d6	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-28	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
49120993-a3a1-4b2d-8ef8-070940c32c50	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-04	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
cef76fba-b86d-4a9a-9023-3b7dfe9594b8	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-04	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
a7a87b1a-f048-42f4-a162-263ef8b4c77e	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-11	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
02528963-eb49-463d-9da6-18eb722d9a8f	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-11	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
17a14ee5-f82d-4368-a532-ee2f73130c31	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-18	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
d62e8c7c-518b-477e-9073-5b707a565456	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-18	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
39d053e7-d56f-488b-8e31-6144a4fde9d8	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-25	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
96033a69-95ad-4536-abc3-ea23cffde2d4	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-25	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
d69aac34-2730-4688-ab2a-d03e2c002f6a	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-10-02	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
f2a3dd6f-686a-4b11-826e-1ac09f290bbe	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-10-02	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
5f7be614-bb33-499c-8260-fe64bcda451b	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-10-09	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
703e5661-0d12-46b7-8acb-cd8eab156003	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-10-09	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
590ba7a5-b594-4e07-b657-98fef89c489c	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-10-16	08:30:00	10:30:00	2.00	lecture	\N	เช็คชื่อ	draft	\N	\N	\N	\N	\N
7d9f8b8c-968a-4eda-94b8-2bdc99ecdda8	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-10-16	10:30:00	12:30:00	2.00	lab	\N	สอนปฏิบัติ	draft	\N	\N	\N	\N	\N
2f2e7ffa-7b0e-4898-9698-1b43d37e40f3	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-06-27	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
b9e9808c-2033-4b38-a721-f88f3d024114	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-04	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
02bf7226-694e-4379-b1d5-33124d633519	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-06-25	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
5c10fcb2-cce0-4644-8179-8524245a97c3	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-02	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
41271b78-c5a7-4460-b1bd-e8cfe31264c2	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-02	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
e3c2a4a5-55fa-4874-b8d8-28b12ccc242d	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-09	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
6c352547-59ca-4717-84ad-1780cc6b8a0b	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-09	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
86e06d36-8d8a-4a6f-95d8-f57b55044e9a	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-16	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
6faaf1c1-5fe9-4529-9cfa-0ab7de0135f6	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-15	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
d9f52788-ecc1-4b77-b1ee-6171e5ec7919	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-11	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
2c7fb3f2-ae9f-4135-8a78-026e32cc6e4c	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-18	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
f93e5371-6b2c-4c44-ae6e-5fbcb75ec5c1	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-07-25	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
2ca97919-9255-4145-a67f-df6757aabc12	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-01	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
907914d0-80a3-4a21-9fab-fbfe5dd7c9d1	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-08	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
80950994-641c-426d-a5b8-6745d9e5d7d1	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-15	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
f2ff7c12-ec8a-4507-8967-c5c201df9684	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-22	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
8efb3e26-26d7-4e62-8e2d-a5906927fbc8	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-08-29	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
945b7f11-2995-4d06-8fa9-c50882ccf57c	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-05	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
4ed84bc4-5556-4071-bee9-0736c583680f	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-12	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
aaf98095-a844-4dcf-97e4-b6ab80ce0796	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-19	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
c2c19dae-ecdd-43cf-8a65-cee4c2933670	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-09-26	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
39667975-b239-461f-90d0-82e1a653d721	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-10-03	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
a338e8ef-867f-4610-bf89-92088785e8f3	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-10-10	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
bec60bc8-9ac8-472b-97fa-0e4230460046	f2238f47-c954-48d3-b7d6-6b88c60f33d5	2026-10-17	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	draft	\N	\N	\N	\N	\N
ea578a4a-fc27-4448-be60-37dee4930682	17cf718e-223d-447e-8974-437a5ef678de	2026-09-24	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
f1785c80-09f7-4a4c-95f9-4af86c774244	17cf718e-223d-447e-8974-437a5ef678de	2026-09-24	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
02622c50-a745-4f7e-ac57-b3733d62e732	17cf718e-223d-447e-8974-437a5ef678de	2026-09-27	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
d8ca741c-8eeb-491e-b61c-fcb6552f9fee	17cf718e-223d-447e-8974-437a5ef678de	2026-10-01	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
06ac4111-6324-47e5-8d2e-20e0d6e42517	17cf718e-223d-447e-8974-437a5ef678de	2026-10-01	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
7df2eb2f-b82f-4560-a82d-e7175a83d4a1	17cf718e-223d-447e-8974-437a5ef678de	2026-10-04	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
d89802e7-6a28-406a-afde-ec3287185f6e	17cf718e-223d-447e-8974-437a5ef678de	2026-10-08	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
adcddbe8-2b67-4781-9908-66eb0a26fdf3	17cf718e-223d-447e-8974-437a5ef678de	2026-10-08	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
eccee0ec-df53-4d05-b14c-e92f16794616	17cf718e-223d-447e-8974-437a5ef678de	2026-10-11	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
2e045df0-2896-4ec6-a9e1-49a45f9c3d79	17cf718e-223d-447e-8974-437a5ef678de	2026-10-15	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
ce851a87-c5bc-4358-b150-e054f4e099a0	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-22	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
f8f0c656-4c4e-4f91-b2de-4aaa90d28be6	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-22	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
68195702-4aa0-40a8-bbe3-578211cf747d	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-29	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
befb1d6d-e00e-4fd0-87a5-2ae7122b3843	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-29	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
ee2b4356-c1f8-4725-9d61-6d8169fc0225	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-04	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
7a53667a-de45-40c9-bad8-a53469ab93b1	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-11	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
971e8a66-1864-4de1-9d3e-77bef34a4b95	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-18	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
a69918e4-7041-47c9-802a-a463d1b5dc6b	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-25	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
c212518e-ddc6-4f05-87ab-3667b6c46cbd	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-01	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
a800611e-ea0b-4d5b-8ece-03347b0958ad	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-19	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
09c57767-4fd9-4150-b831-2ba1e06bf726	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-26	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
4c1cfbc6-081d-42cb-8d92-b9f736576021	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-03	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
3651f9ff-eccc-4a37-b45f-64895ad7bd26	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-10	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
de1e4ffd-cb21-4946-8afb-5d03fe5852b8	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-17	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
35ab8326-5a10-4401-9c8b-834874e9fce9	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-24	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
b56edd6b-1e9a-4a51-9e7b-83aac9e2a6b9	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-31	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
6beec974-6fd0-4898-8036-f5effaa90f46	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-06	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
0e6e4799-d08d-4d94-9a1a-e8a84d4b0334	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-13	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
5f4dc66a-a75b-4ec7-b16b-a9fd6898283d	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-13	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
4fbd077e-9d13-4ad3-a10d-4c928799d340	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-20	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
ed0818c7-16f3-448d-ae87-272f1c46a9ca	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-20	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
a7bf03bf-1f8d-46fe-bcc1-052e20c65c53	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-27	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
451253b7-9efb-4f00-8548-fcce93fd84a8	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-27	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
19c664fd-f1a3-46fc-992e-e556a408fd9d	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-03	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
b1ab0120-ce7e-432e-9778-fcd2c241f5e6	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-03	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
ca29bf7c-c6bf-4877-b95b-cca4644ea024	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-10	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
a4142614-6651-481f-a8a9-c67b4f64a8a4	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-10	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
4f6e2563-b086-4cf8-8859-73ebc6f279c2	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-17	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
c842ca95-e3b9-4f14-8a9c-91aa3cb770dc	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-17	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
d10d8007-1c84-4d55-bb7b-35b443898047	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-16	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
f4daa4a5-022d-40b3-94d5-59d97ccce59d	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-23	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
a604b3a6-da53-496a-ad6f-1c0ba15f4fa1	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-23	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
d0959f58-af47-445f-8243-4f9e22b296aa	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-30	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
c36defe5-450f-45c8-bccc-35168ee59464	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-07-30	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
0a97b330-c3fc-4741-9d7b-7e8e24ea3d34	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-06	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
61e0be8b-a76c-4f67-a7e1-6ce245c5d89c	17cf718e-223d-447e-8974-437a5ef678de	2026-10-15	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
21aa7ba1-9a22-48b6-b179-140a7afab56d	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-06-27	14:00:00	16:00:00	2.00	review	\N	\N	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
a5842044-5bf5-4153-b3de-b33d28b9f0a3	17cf718e-223d-447e-8974-437a5ef678de	2026-06-25	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
4c691bd0-6fc4-4401-9fc5-50976a9789ab	17cf718e-223d-447e-8974-437a5ef678de	2026-06-25	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
c91ea5bd-ab74-49ee-8895-ed712bd6f101	17cf718e-223d-447e-8974-437a5ef678de	2026-06-28	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
64dd276a-09da-4bad-8292-8851b5ed7182	17cf718e-223d-447e-8974-437a5ef678de	2026-07-02	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
50ffe17d-4874-4bd1-b098-590890fb7288	17cf718e-223d-447e-8974-437a5ef678de	2026-07-02	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
7f5e7a1a-8cf2-42bb-b834-d1fc325097bd	17cf718e-223d-447e-8974-437a5ef678de	2026-07-05	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
d83d0885-6725-4896-a24a-6f29c23a4a74	17cf718e-223d-447e-8974-437a5ef678de	2026-07-09	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
c934f87c-bc16-490a-8378-8a75dce033a3	17cf718e-223d-447e-8974-437a5ef678de	2026-07-09	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
124b59ca-9486-494a-8805-cedbe55cadbe	17cf718e-223d-447e-8974-437a5ef678de	2026-07-12	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
228c4433-8804-4028-88b5-759403c0fefe	17cf718e-223d-447e-8974-437a5ef678de	2026-07-16	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
6482096f-01a5-47b6-afd7-1d973efad0cf	17cf718e-223d-447e-8974-437a5ef678de	2026-07-16	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
87a30d95-e2de-4e9e-860a-4baaf81330fd	17cf718e-223d-447e-8974-437a5ef678de	2026-07-19	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
f76ce9c3-3fc4-4657-9cef-389ceb7b2e98	17cf718e-223d-447e-8974-437a5ef678de	2026-07-23	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
43230e2a-393a-40b3-ae4b-807972f862f9	17cf718e-223d-447e-8974-437a5ef678de	2026-07-23	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
9b80aa98-4154-4ac3-85d2-0947f7a8b885	17cf718e-223d-447e-8974-437a5ef678de	2026-07-26	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
426cd434-5868-405a-9079-bf283e5770b8	17cf718e-223d-447e-8974-437a5ef678de	2026-07-30	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
e4bffcf6-7ef3-4756-93fe-b79ad5d1ea0e	17cf718e-223d-447e-8974-437a5ef678de	2026-07-30	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
4f83ac71-5401-4b17-bd38-97468585a529	17cf718e-223d-447e-8974-437a5ef678de	2026-08-02	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
100ec255-5ffb-43bb-a25b-30d91f571ea5	17cf718e-223d-447e-8974-437a5ef678de	2026-08-06	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
af140593-c5c0-4d84-bec0-a3f39caeb89e	17cf718e-223d-447e-8974-437a5ef678de	2026-08-06	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
2a0ace8f-d6a8-4d20-816f-e45a69477c26	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-24	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
bca8cc6f-0baa-4069-8912-bcf7ee526e38	17cf718e-223d-447e-8974-437a5ef678de	2026-08-09	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
614487b0-d809-4843-9198-4ca126668859	17cf718e-223d-447e-8974-437a5ef678de	2026-08-13	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
a938bf86-128b-49e9-916f-7c1fdba618df	17cf718e-223d-447e-8974-437a5ef678de	2026-08-13	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
4e9e296c-4e09-46ed-bb87-c1b50b08796a	17cf718e-223d-447e-8974-437a5ef678de	2026-08-16	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
7f0497b8-8cbf-462e-8d1b-36dddb38992a	17cf718e-223d-447e-8974-437a5ef678de	2026-08-20	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
3beee1ce-1237-47a0-9636-e1a201361b39	17cf718e-223d-447e-8974-437a5ef678de	2026-08-20	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
87e4980f-0eac-4452-b6fe-1e61e4993aef	17cf718e-223d-447e-8974-437a5ef678de	2026-08-23	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
d30082af-9952-40d7-b7d4-45d648bf9e2d	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-24	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
05f3e792-6570-40fe-bf82-08a2e96741c3	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-01	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
3eab56e4-4d4d-4c5e-aa3c-1a6e7c94478d	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-01	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
a21d6b1e-3b7a-46b0-ac3f-ac6a1cefa84a	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-08	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
2588ec57-8ae7-4b3c-94a4-4e136b001c5f	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-08	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
da36f8c1-60e3-4461-ba28-4d51cfaf4bdc	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-10-15	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
efcc4556-bfe2-4f50-8db9-8f746a5ac9cf	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-06-25	10:30:00	12:30:00	2.00	lecture		เช็คชื่อ	approved	2026-07-15 12:54:17.098329+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	ดูข้อความหมายเหตุของวันที่ 25/06 ใหม่	\N
1560f386-7743-4914-b5c6-828098d32258	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-08	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
1ebe771a-1cab-4a33-ab12-d39531e629a4	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-15	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
09b0451d-1a7e-407d-9e19-c14f3a2cf1c8	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-22	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
50c03d8a-23c9-409e-b103-f36767a60f57	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-08-29	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
ed7fe679-df7c-47df-bed4-68b5f9c2beaf	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-05	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
cde6f495-f7ee-4594-8995-567e48cae94c	96f9b8d4-abc0-4372-807f-f0617bf610e7	2026-09-12	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	approved	2026-07-15 13:00:17.963761+07	2026-07-15 13:00:29.569093+07	7a3d60b5-5091-40e2-b18a-2cf58f2b5d55	\N	\N
d0ff51ba-b028-4a63-a452-35a4ab31a4c9	17cf718e-223d-447e-8974-437a5ef678de	2026-08-27	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
bb6b870d-2cc7-42de-b572-d1c650a4ff2c	17cf718e-223d-447e-8974-437a5ef678de	2026-08-27	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
df042bc5-98b9-40f3-8beb-2539a7722fc3	17cf718e-223d-447e-8974-437a5ef678de	2026-08-30	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
9fbafc4a-ab2b-4d26-856e-34d1ae93a441	17cf718e-223d-447e-8974-437a5ef678de	2026-09-03	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
7eaaa15b-2d15-4554-9172-d2a0eeaaa140	17cf718e-223d-447e-8974-437a5ef678de	2026-09-03	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
22a7f1f9-b7dd-40df-91f6-8fa2f8a5ca7a	17cf718e-223d-447e-8974-437a5ef678de	2026-09-06	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
4ae5d263-f4d8-4906-9419-8fbeaa94b3f3	17cf718e-223d-447e-8974-437a5ef678de	2026-09-10	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
3752eeb6-adc1-4934-bc25-59a08a8ca295	17cf718e-223d-447e-8974-437a5ef678de	2026-09-10	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
72507f41-06d6-418c-9b1b-f60dbaa1a87a	17cf718e-223d-447e-8974-437a5ef678de	2026-09-13	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
4b199de0-2dca-4a2b-a64c-4dc0524cf261	17cf718e-223d-447e-8974-437a5ef678de	2026-09-17	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
c4b6cddf-1443-4bb5-a6d8-500c6167c62f	17cf718e-223d-447e-8974-437a5ef678de	2026-09-17	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
76130491-e2c1-416c-9ef7-3813f89c0a6c	17cf718e-223d-447e-8974-437a5ef678de	2026-09-20	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
3cdde72a-0b0b-415f-9e01-7adc5c840372	17cf718e-223d-447e-8974-437a5ef678de	2026-10-18	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
cee401ce-14a1-4347-ba9d-a41adfbd116b	17cf718e-223d-447e-8974-437a5ef678de	2026-10-22	15:00:00	17:00:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
e45aea4f-ad0c-4920-a639-13785fa4386a	17cf718e-223d-447e-8974-437a5ef678de	2026-10-22	17:00:00	19:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 00:43:31.721989+07	\N	\N	\N	\N
1fb09c92-b917-4d11-82ac-1aa6f0180058	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-06-24	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
d450ddb4-1071-4822-bab7-ca5f50a94dd3	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-06-24	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
6709b085-8164-40a1-88d2-92fc8f8e303c	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-06-27	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
743b2c68-de7d-47ae-869e-5adc2a4a6f8c	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-28	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
dfa3877a-79bb-4e56-99c9-d2cf525df9f8	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-28	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
36d81e63-97d5-4a37-9094-b27db642821c	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-31	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
c0d60b7c-d32f-4a16-830a-c8013907fe8a	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-01	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
2b4ae0af-6908-46f0-9cd6-23b305e664f9	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-01	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
d50b8d43-dfa4-48ac-940b-6ad564e407ac	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-04	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
3afd592f-b30a-4ecf-a89a-b9d21bdc1be4	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-08	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
08d584fa-c56e-4f0a-a8d6-7c24c4db4001	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-08	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
0bc7fd81-8229-49f7-84ac-9429949ae494	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-11	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
e844ff37-63d9-4049-80cd-8a9aad3776ce	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-15	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
026794a4-62da-4836-9815-573df264ffae	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-15	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
b82a1785-5ed7-4b83-85d4-302622040ee7	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-18	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
80287959-cd84-4fc8-b0dd-5befb359cbeb	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-22	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
9a6c849a-165f-491c-aec5-321c95e082e5	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-22	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
4090cf00-2b21-4ab3-a0d5-6d84eb8bd265	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-07-25	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
b8db620e-d176-4171-960a-86dd770977d4	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-01	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
5fafa6d5-2637-44f4-a191-dcd7cf75ca8c	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-05	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
75825f62-8550-43fc-a238-299a89f86d5e	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-05	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
69b95e91-ecbf-4e9d-8522-c56a78874637	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-08	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
f4a8ee7f-ddaf-435f-82da-5bd2614bfc44	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-15	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
9fe28040-2cd2-452d-af78-3f2e69c73727	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-19	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
6439a3d8-e6aa-4c4b-b7f3-2f88693ed9ea	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-19	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
82d915d6-2e58-4b35-8fa0-31ff0a01c774	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-22	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
03070848-b42f-437b-bbaf-b1916c2ce7f6	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-26	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
15b55741-1445-470d-8625-cce74de0fb79	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-26	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
a5979c07-e1ed-434b-8188-c3df2cd1dd1d	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-08-29	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
ae80effd-e4c8-426f-9fba-0b7a1dc879a5	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-02	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
a95242db-1163-4b92-a82e-8d1c87348b47	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-02	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
128c9580-e7d7-433c-81e6-a09c59e17996	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-05	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
8d991f15-5033-44b6-8bfc-665f92128018	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-09	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
845e0284-e8f1-41c4-8280-90711f0e68a7	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-09	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
fa64a9f8-8979-40db-b583-f67b7bcb669a	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-12	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
f857d756-78c8-4113-9256-ce8b3e46edb4	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-16	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
1354bda1-82dc-42c3-a2f1-1c3e40a0af1f	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-16	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
400b9a1c-be4c-4e2d-b7a0-03d0fd77a0ef	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-19	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
762332e1-1f0d-4ab5-af76-4081d7bea0ac	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-23	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
a96245ad-0c97-4d13-a98e-39d8f8b03ba5	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-23	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
aa5c51a5-0396-4466-8c87-b701cdf7576a	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-26	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
f95b0f47-7d44-4e1f-8eb7-53d34d608b90	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-30	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
7ceb7a1e-c07b-4d6b-91a1-8a96e6caa5b2	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-09-30	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
ee17b07e-5b42-491f-8a8e-1ccdd085b88b	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-03	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
e2b2f62d-2c58-427a-bcc5-2432eb1c4499	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-07	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
b7f445f6-5fb5-4400-81fd-f4e6926fd19c	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-07	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
fe9e1b29-db28-416f-8419-5e637393a449	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-10	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
c40ab5ab-2086-4f58-bbe1-8132dc67da3f	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-14	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
87433758-c68b-4cc3-bd9c-0a9db2a46b09	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-14	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
0e98af74-22c6-4ab6-96f9-fea9ef5693c8	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-17	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
3facd334-e876-41d0-8304-28e5e9a1b31b	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-21	10:30:00	12:30:00	2.00	lecture	\N	เช็คชื่อ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
fee8e776-81d3-4b50-b47c-90467c5718a5	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-21	13:00:00	15:00:00	2.00	lab	\N	สอนปฏิบัติ	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
f3960dad-7b01-4084-ada4-d58b018f79bd	69048d9c-db89-4fc0-9f18-b2f84f04ffaa	2026-10-24	14:00:00	16:00:00	2.00	review	\N	ตรวจงาน	submitted	2026-07-16 16:40:33.135+07	\N	\N	\N	\N
\.


--
-- Name: audit_logs_id_seq; Type: SEQUENCE SET; Schema: public; Owner: itii_database_prod
--

SELECT pg_catalog.setval('public.audit_logs_id_seq', 517, true);


--
-- Name: academic_terms academic_terms_academic_year_semester_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.academic_terms
    ADD CONSTRAINT academic_terms_academic_year_semester_key UNIQUE (academic_year, semester);


--
-- Name: academic_terms academic_terms_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.academic_terms
    ADD CONSTRAINT academic_terms_pkey PRIMARY KEY (id);


--
-- Name: admin_officers admin_officers_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.admin_officers
    ADD CONSTRAINT admin_officers_pkey PRIMARY KEY (id);


--
-- Name: announcements announcements_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.announcements
    ADD CONSTRAINT announcements_pkey PRIMARY KEY (id);


--
-- Name: audit_logs audit_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.audit_logs
    ADD CONSTRAINT audit_logs_pkey PRIMARY KEY (id);


--
-- Name: budget_caps budget_caps_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.budget_caps
    ADD CONSTRAINT budget_caps_pkey PRIMARY KEY (id);


--
-- Name: document_progress document_progress_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.document_progress
    ADD CONSTRAINT document_progress_pkey PRIMARY KEY (term_id);


--
-- Name: exam_schedules exam_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.exam_schedules
    ADD CONSTRAINT exam_schedules_pkey PRIMARY KEY (id);


--
-- Name: export_batches export_batches_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.export_batches
    ADD CONSTRAINT export_batches_pkey PRIMARY KEY (id);


--
-- Name: exports exports_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.exports
    ADD CONSTRAINT exports_pkey PRIMARY KEY (id);


--
-- Name: holiday_remind_log holiday_remind_log_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.holiday_remind_log
    ADD CONSTRAINT holiday_remind_log_pkey PRIMARY KEY (id);


--
-- Name: lecture_review_dates lecture_review_dates_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.lecture_review_dates
    ADD CONSTRAINT lecture_review_dates_pkey PRIMARY KEY (id);


--
-- Name: makeup_schedules makeup_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.makeup_schedules
    ADD CONSTRAINT makeup_schedules_pkey PRIMARY KEY (id);


--
-- Name: notifications notifications_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.notifications
    ADD CONSTRAINT notifications_pkey PRIMARY KEY (id);


--
-- Name: pay_rates pay_rates_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.pay_rates
    ADD CONSTRAINT pay_rates_pkey PRIMARY KEY (id);


--
-- Name: public_holidays public_holidays_holiday_date_source_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.public_holidays
    ADD CONSTRAINT public_holidays_holiday_date_source_key UNIQUE (holiday_date, source);


--
-- Name: public_holidays public_holidays_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.public_holidays
    ADD CONSTRAINT public_holidays_pkey PRIMARY KEY (id);


--
-- Name: schedule_imports schedule_imports_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.schedule_imports
    ADD CONSTRAINT schedule_imports_pkey PRIMARY KEY (id);


--
-- Name: schema_migrations schema_migrations_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.schema_migrations
    ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (version);


--
-- Name: section_schedules section_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.section_schedules
    ADD CONSTRAINT section_schedules_pkey PRIMARY KEY (id);


--
-- Name: sections sections_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.sections
    ADD CONSTRAINT sections_pkey PRIMARY KEY (id);


--
-- Name: sections sections_teaching_course_id_sec_no_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.sections
    ADD CONSTRAINT sections_teaching_course_id_sec_no_key UNIQUE (teaching_course_id, sec_no);


--
-- Name: signature_checklist signature_checklist_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.signature_checklist
    ADD CONSTRAINT signature_checklist_pkey PRIMARY KEY (id);


--
-- Name: signature_checklist signature_checklist_teaching_course_id_role_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.signature_checklist
    ADD CONSTRAINT signature_checklist_teaching_course_id_role_key UNIQUE (teaching_course_id, role);


--
-- Name: submission_period_status submission_period_status_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_period_status
    ADD CONSTRAINT submission_period_status_pkey PRIMARY KEY (id);


--
-- Name: submission_period_status submission_period_status_submission_period_id_ta_id_teachin_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_period_status
    ADD CONSTRAINT submission_period_status_submission_period_id_ta_id_teachin_key UNIQUE (submission_period_id, ta_id, teaching_course_id);


--
-- Name: submission_periods submission_periods_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_periods
    ADD CONSTRAINT submission_periods_pkey PRIMARY KEY (id);


--
-- Name: submission_periods submission_periods_term_id_year_month_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_periods
    ADD CONSTRAINT submission_periods_term_id_year_month_key UNIQUE (term_id, year_month);


--
-- Name: ta_class_schedules ta_class_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_class_schedules
    ADD CONSTRAINT ta_class_schedules_pkey PRIMARY KEY (id);


--
-- Name: ta_documents ta_documents_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_documents
    ADD CONSTRAINT ta_documents_pkey PRIMARY KEY (id);


--
-- Name: ta_profile_submissions ta_profile_submissions_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_profile_submissions
    ADD CONSTRAINT ta_profile_submissions_pkey PRIMARY KEY (id);


--
-- Name: ta_profile_submissions ta_profile_submissions_user_id_round_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_profile_submissions
    ADD CONSTRAINT ta_profile_submissions_user_id_round_key UNIQUE (user_id, round);


--
-- Name: ta_profiles ta_profiles_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_profiles
    ADD CONSTRAINT ta_profiles_pkey PRIMARY KEY (user_id);


--
-- Name: ta_request_assignments ta_request_assignments_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_request_assignments
    ADD CONSTRAINT ta_request_assignments_pkey PRIMARY KEY (id);


--
-- Name: ta_request_assignments ta_request_assignments_request_id_section_id_ta_id_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_request_assignments
    ADD CONSTRAINT ta_request_assignments_request_id_section_id_ta_id_key UNIQUE (request_id, section_id, ta_id);


--
-- Name: ta_request_counts ta_request_counts_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_request_counts
    ADD CONSTRAINT ta_request_counts_pkey PRIMARY KEY (request_id, section_id);


--
-- Name: ta_request_windows ta_request_windows_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_request_windows
    ADD CONSTRAINT ta_request_windows_pkey PRIMARY KEY (id);


--
-- Name: ta_requests ta_requests_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_requests
    ADD CONSTRAINT ta_requests_pkey PRIMARY KEY (id);


--
-- Name: ta_review_schedules ta_review_schedules_assignment_id_day_of_week_start_time_en_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_review_schedules
    ADD CONSTRAINT ta_review_schedules_assignment_id_day_of_week_start_time_en_key UNIQUE (assignment_id, day_of_week, start_time, end_time);


--
-- Name: ta_review_schedules ta_review_schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_review_schedules
    ADD CONSTRAINT ta_review_schedules_pkey PRIMARY KEY (id);


--
-- Name: ta_workload_forms ta_workload_forms_assignment_id_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_workload_forms
    ADD CONSTRAINT ta_workload_forms_assignment_id_key UNIQUE (assignment_id);


--
-- Name: ta_workload_forms ta_workload_forms_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_workload_forms
    ADD CONSTRAINT ta_workload_forms_pkey PRIMARY KEY (id);


--
-- Name: teaching_courses teaching_courses_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.teaching_courses
    ADD CONSTRAINT teaching_courses_pkey PRIMARY KEY (id);


--
-- Name: teaching_courses teaching_courses_term_id_code_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.teaching_courses
    ADD CONSTRAINT teaching_courses_term_id_code_key UNIQUE (term_id, code);


--
-- Name: teaching_lecturers teaching_lecturers_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.teaching_lecturers
    ADD CONSTRAINT teaching_lecturers_pkey PRIMARY KEY (teaching_course_id, lecturer_id);


--
-- Name: makeup_schedules uq_makeup_section_original; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.makeup_schedules
    ADD CONSTRAINT uq_makeup_section_original UNIQUE (section_id, original_date);


--
-- Name: user_roles user_roles_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.user_roles
    ADD CONSTRAINT user_roles_pkey PRIMARY KEY (user_id, role);


--
-- Name: users users_email_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_email_key UNIQUE (email);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: users users_sso_subject_key; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_sso_subject_key UNIQUE (sso_subject);


--
-- Name: work_logs work_logs_pkey; Type: CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.work_logs
    ADD CONSTRAINT work_logs_pkey PRIMARY KEY (id);


--
-- Name: announcements_pending_fanout_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX announcements_pending_fanout_idx ON public.announcements USING btree (published_at) WHERE ((announced_at IS NULL) AND (published_at IS NOT NULL));


--
-- Name: announcements_pub_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX announcements_pub_idx ON public.announcements USING btree (published_at DESC NULLS LAST) WHERE (published_at IS NOT NULL);


--
-- Name: audit_actor_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX audit_actor_idx ON public.audit_logs USING btree (actor_id, at DESC);


--
-- Name: audit_at_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX audit_at_idx ON public.audit_logs USING btree (at DESC);


--
-- Name: audit_entity_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX audit_entity_idx ON public.audit_logs USING btree (entity, entity_id);


--
-- Name: export_batches_course_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX export_batches_course_idx ON public.export_batches USING btree (teaching_course_id, generated_at DESC);


--
-- Name: export_batches_period_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX export_batches_period_idx ON public.export_batches USING btree (submission_period_id);


--
-- Name: ix_hrl_recent; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX ix_hrl_recent ON public.holiday_remind_log USING btree (ta_id, teaching_course_id, original_date, sent_at DESC);


--
-- Name: ix_public_holidays_date; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX ix_public_holidays_date ON public.public_holidays USING btree (holiday_date);


--
-- Name: ix_signature_checklist_term; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX ix_signature_checklist_term ON public.signature_checklist USING btree (term_id);


--
-- Name: ix_trs_assignment; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX ix_trs_assignment ON public.ta_review_schedules USING btree (assignment_id);


--
-- Name: notifications_user_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX notifications_user_idx ON public.notifications USING btree (user_id, created_at DESC);


--
-- Name: section_schedules_section_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX section_schedules_section_idx ON public.section_schedules USING btree (section_id);


--
-- Name: submission_period_status_lock_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX submission_period_status_lock_idx ON public.submission_period_status USING btree (ta_id, teaching_course_id) WHERE (status = ANY (ARRAY['exported'::text, 'finance_sent'::text]));


--
-- Name: submission_period_status_ta_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX submission_period_status_ta_idx ON public.submission_period_status USING btree (ta_id, status);


--
-- Name: submission_periods_due_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX submission_periods_due_idx ON public.submission_periods USING btree (due_date, is_closed);


--
-- Name: ta_class_user_term_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX ta_class_user_term_idx ON public.ta_class_schedules USING btree (user_id, term_id);


--
-- Name: ta_documents_current_uidx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE UNIQUE INDEX ta_documents_current_uidx ON public.ta_documents USING btree (user_id, kind) WHERE (superseded_at IS NULL);


--
-- Name: ta_documents_expiry_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX ta_documents_expiry_idx ON public.ta_documents USING btree (expires_at) WHERE ((expires_at IS NOT NULL) AND (file_deleted_at IS NULL));


--
-- Name: ta_documents_user_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX ta_documents_user_idx ON public.ta_documents USING btree (user_id, kind);


--
-- Name: ta_profile_submissions_user_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX ta_profile_submissions_user_idx ON public.ta_profile_submissions USING btree (user_id, submitted_at DESC);


--
-- Name: users_active_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX users_active_idx ON public.users USING btree (is_active) WHERE (deleted_at IS NULL);


--
-- Name: work_logs_assign_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX work_logs_assign_idx ON public.work_logs USING btree (assignment_id, work_date);


--
-- Name: work_logs_assignment_date_idx; Type: INDEX; Schema: public; Owner: itii_database_prod
--

CREATE INDEX work_logs_assignment_date_idx ON public.work_logs USING btree (assignment_id, work_date);


--
-- Name: announcements announcements_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.announcements
    ADD CONSTRAINT announcements_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id);


--
-- Name: announcements announcements_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.announcements
    ADD CONSTRAINT announcements_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES public.users(id);


--
-- Name: audit_logs audit_logs_actor_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.audit_logs
    ADD CONSTRAINT audit_logs_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES public.users(id);


--
-- Name: document_progress document_progress_term_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.document_progress
    ADD CONSTRAINT document_progress_term_id_fkey FOREIGN KEY (term_id) REFERENCES public.academic_terms(id) ON DELETE CASCADE;


--
-- Name: document_progress document_progress_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.document_progress
    ADD CONSTRAINT document_progress_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES public.users(id);


--
-- Name: exam_schedules exam_schedules_section_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.exam_schedules
    ADD CONSTRAINT exam_schedules_section_id_fkey FOREIGN KEY (section_id) REFERENCES public.sections(id) ON DELETE CASCADE;


--
-- Name: export_batches export_batches_generated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.export_batches
    ADD CONSTRAINT export_batches_generated_by_fkey FOREIGN KEY (generated_by) REFERENCES public.users(id) ON DELETE RESTRICT;


--
-- Name: export_batches export_batches_submission_period_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.export_batches
    ADD CONSTRAINT export_batches_submission_period_id_fkey FOREIGN KEY (submission_period_id) REFERENCES public.submission_periods(id) ON DELETE SET NULL;


--
-- Name: export_batches export_batches_teaching_course_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.export_batches
    ADD CONSTRAINT export_batches_teaching_course_id_fkey FOREIGN KEY (teaching_course_id) REFERENCES public.teaching_courses(id) ON DELETE CASCADE;


--
-- Name: exports exports_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.exports
    ADD CONSTRAINT exports_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id);


--
-- Name: exports exports_term_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.exports
    ADD CONSTRAINT exports_term_id_fkey FOREIGN KEY (term_id) REFERENCES public.academic_terms(id);


--
-- Name: holiday_remind_log holiday_remind_log_ta_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.holiday_remind_log
    ADD CONSTRAINT holiday_remind_log_ta_id_fkey FOREIGN KEY (ta_id) REFERENCES public.users(id);


--
-- Name: holiday_remind_log holiday_remind_log_teaching_course_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.holiday_remind_log
    ADD CONSTRAINT holiday_remind_log_teaching_course_id_fkey FOREIGN KEY (teaching_course_id) REFERENCES public.teaching_courses(id);


--
-- Name: lecture_review_dates lecture_review_dates_section_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.lecture_review_dates
    ADD CONSTRAINT lecture_review_dates_section_id_fkey FOREIGN KEY (section_id) REFERENCES public.sections(id) ON DELETE CASCADE;


--
-- Name: makeup_schedules makeup_schedules_section_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.makeup_schedules
    ADD CONSTRAINT makeup_schedules_section_id_fkey FOREIGN KEY (section_id) REFERENCES public.sections(id) ON DELETE CASCADE;


--
-- Name: notifications notifications_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.notifications
    ADD CONSTRAINT notifications_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: public_holidays public_holidays_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.public_holidays
    ADD CONSTRAINT public_holidays_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id);


--
-- Name: schedule_imports schedule_imports_imported_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.schedule_imports
    ADD CONSTRAINT schedule_imports_imported_by_fkey FOREIGN KEY (imported_by) REFERENCES public.users(id);


--
-- Name: section_schedules section_schedules_section_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.section_schedules
    ADD CONSTRAINT section_schedules_section_id_fkey FOREIGN KEY (section_id) REFERENCES public.sections(id) ON DELETE CASCADE;


--
-- Name: sections sections_teaching_course_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.sections
    ADD CONSTRAINT sections_teaching_course_id_fkey FOREIGN KEY (teaching_course_id) REFERENCES public.teaching_courses(id) ON DELETE CASCADE;


--
-- Name: signature_checklist signature_checklist_teaching_course_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.signature_checklist
    ADD CONSTRAINT signature_checklist_teaching_course_id_fkey FOREIGN KEY (teaching_course_id) REFERENCES public.teaching_courses(id) ON DELETE CASCADE;


--
-- Name: signature_checklist signature_checklist_term_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.signature_checklist
    ADD CONSTRAINT signature_checklist_term_id_fkey FOREIGN KEY (term_id) REFERENCES public.academic_terms(id) ON DELETE CASCADE;


--
-- Name: signature_checklist signature_checklist_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.signature_checklist
    ADD CONSTRAINT signature_checklist_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES public.users(id);


--
-- Name: submission_period_status submission_period_status_exported_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_period_status
    ADD CONSTRAINT submission_period_status_exported_by_fkey FOREIGN KEY (exported_by) REFERENCES public.users(id);


--
-- Name: submission_period_status submission_period_status_finance_sent_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_period_status
    ADD CONSTRAINT submission_period_status_finance_sent_by_fkey FOREIGN KEY (finance_sent_by) REFERENCES public.users(id);


--
-- Name: submission_period_status submission_period_status_lecturer_signed_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_period_status
    ADD CONSTRAINT submission_period_status_lecturer_signed_by_fkey FOREIGN KEY (lecturer_signed_by) REFERENCES public.users(id);


--
-- Name: submission_period_status submission_period_status_sent_back_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_period_status
    ADD CONSTRAINT submission_period_status_sent_back_by_fkey FOREIGN KEY (sent_back_by) REFERENCES public.users(id);


--
-- Name: submission_period_status submission_period_status_staff_reviewed_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_period_status
    ADD CONSTRAINT submission_period_status_staff_reviewed_by_fkey FOREIGN KEY (staff_reviewed_by) REFERENCES public.users(id);


--
-- Name: submission_period_status submission_period_status_submission_period_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_period_status
    ADD CONSTRAINT submission_period_status_submission_period_id_fkey FOREIGN KEY (submission_period_id) REFERENCES public.submission_periods(id) ON DELETE CASCADE;


--
-- Name: submission_period_status submission_period_status_ta_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_period_status
    ADD CONSTRAINT submission_period_status_ta_id_fkey FOREIGN KEY (ta_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: submission_period_status submission_period_status_teaching_course_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_period_status
    ADD CONSTRAINT submission_period_status_teaching_course_id_fkey FOREIGN KEY (teaching_course_id) REFERENCES public.teaching_courses(id) ON DELETE CASCADE;


--
-- Name: submission_periods submission_periods_term_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.submission_periods
    ADD CONSTRAINT submission_periods_term_id_fkey FOREIGN KEY (term_id) REFERENCES public.academic_terms(id) ON DELETE CASCADE;


--
-- Name: ta_class_schedules ta_class_schedules_term_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_class_schedules
    ADD CONSTRAINT ta_class_schedules_term_id_fkey FOREIGN KEY (term_id) REFERENCES public.academic_terms(id);


--
-- Name: ta_class_schedules ta_class_schedules_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_class_schedules
    ADD CONSTRAINT ta_class_schedules_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: ta_documents ta_documents_reviewed_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_documents
    ADD CONSTRAINT ta_documents_reviewed_by_fkey FOREIGN KEY (reviewed_by) REFERENCES public.users(id);


--
-- Name: ta_documents ta_documents_superseded_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_documents
    ADD CONSTRAINT ta_documents_superseded_by_fkey FOREIGN KEY (superseded_by) REFERENCES public.ta_documents(id) ON DELETE SET NULL;


--
-- Name: ta_documents ta_documents_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_documents
    ADD CONSTRAINT ta_documents_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: ta_profile_submissions ta_profile_submissions_reviewed_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_profile_submissions
    ADD CONSTRAINT ta_profile_submissions_reviewed_by_fkey FOREIGN KEY (reviewed_by) REFERENCES public.users(id);


--
-- Name: ta_profile_submissions ta_profile_submissions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_profile_submissions
    ADD CONSTRAINT ta_profile_submissions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: ta_profiles ta_profiles_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_profiles
    ADD CONSTRAINT ta_profiles_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: ta_profiles ta_profiles_verified_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_profiles
    ADD CONSTRAINT ta_profiles_verified_by_fkey FOREIGN KEY (verified_by) REFERENCES public.users(id);


--
-- Name: ta_request_assignments ta_request_assignments_request_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_request_assignments
    ADD CONSTRAINT ta_request_assignments_request_id_fkey FOREIGN KEY (request_id) REFERENCES public.ta_requests(id) ON DELETE CASCADE;


--
-- Name: ta_request_assignments ta_request_assignments_section_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_request_assignments
    ADD CONSTRAINT ta_request_assignments_section_id_fkey FOREIGN KEY (section_id) REFERENCES public.sections(id);


--
-- Name: ta_request_assignments ta_request_assignments_ta_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_request_assignments
    ADD CONSTRAINT ta_request_assignments_ta_id_fkey FOREIGN KEY (ta_id) REFERENCES public.users(id);


--
-- Name: ta_request_counts ta_request_counts_request_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_request_counts
    ADD CONSTRAINT ta_request_counts_request_id_fkey FOREIGN KEY (request_id) REFERENCES public.ta_requests(id) ON DELETE CASCADE;


--
-- Name: ta_request_counts ta_request_counts_section_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_request_counts
    ADD CONSTRAINT ta_request_counts_section_id_fkey FOREIGN KEY (section_id) REFERENCES public.sections(id);


--
-- Name: ta_request_windows ta_request_windows_term_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_request_windows
    ADD CONSTRAINT ta_request_windows_term_id_fkey FOREIGN KEY (term_id) REFERENCES public.academic_terms(id) ON DELETE CASCADE;


--
-- Name: ta_requests ta_requests_decided_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_requests
    ADD CONSTRAINT ta_requests_decided_by_fkey FOREIGN KEY (decided_by) REFERENCES public.users(id);


--
-- Name: ta_requests ta_requests_lecturer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_requests
    ADD CONSTRAINT ta_requests_lecturer_id_fkey FOREIGN KEY (lecturer_id) REFERENCES public.users(id);


--
-- Name: ta_requests ta_requests_teaching_course_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_requests
    ADD CONSTRAINT ta_requests_teaching_course_id_fkey FOREIGN KEY (teaching_course_id) REFERENCES public.teaching_courses(id);


--
-- Name: ta_requests ta_requests_window_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_requests
    ADD CONSTRAINT ta_requests_window_id_fkey FOREIGN KEY (window_id) REFERENCES public.ta_request_windows(id);


--
-- Name: ta_review_schedules ta_review_schedules_assignment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_review_schedules
    ADD CONSTRAINT ta_review_schedules_assignment_id_fkey FOREIGN KEY (assignment_id) REFERENCES public.ta_request_assignments(id) ON DELETE CASCADE;


--
-- Name: ta_workload_forms ta_workload_forms_assignment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.ta_workload_forms
    ADD CONSTRAINT ta_workload_forms_assignment_id_fkey FOREIGN KEY (assignment_id) REFERENCES public.ta_request_assignments(id) ON DELETE CASCADE;


--
-- Name: teaching_courses teaching_courses_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.teaching_courses
    ADD CONSTRAINT teaching_courses_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id);


--
-- Name: teaching_courses teaching_courses_term_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.teaching_courses
    ADD CONSTRAINT teaching_courses_term_id_fkey FOREIGN KEY (term_id) REFERENCES public.academic_terms(id);


--
-- Name: teaching_lecturers teaching_lecturers_lecturer_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.teaching_lecturers
    ADD CONSTRAINT teaching_lecturers_lecturer_id_fkey FOREIGN KEY (lecturer_id) REFERENCES public.users(id);


--
-- Name: teaching_lecturers teaching_lecturers_teaching_course_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.teaching_lecturers
    ADD CONSTRAINT teaching_lecturers_teaching_course_id_fkey FOREIGN KEY (teaching_course_id) REFERENCES public.teaching_courses(id) ON DELETE CASCADE;


--
-- Name: user_roles user_roles_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.user_roles
    ADD CONSTRAINT user_roles_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;


--
-- Name: work_logs work_logs_approved_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.work_logs
    ADD CONSTRAINT work_logs_approved_by_fkey FOREIGN KEY (approved_by) REFERENCES public.users(id);


--
-- Name: work_logs work_logs_assignment_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: itii_database_prod
--

ALTER TABLE ONLY public.work_logs
    ADD CONSTRAINT work_logs_assignment_id_fkey FOREIGN KEY (assignment_id) REFERENCES public.ta_request_assignments(id) ON DELETE CASCADE;


--
-- PostgreSQL database dump complete
--

\unrestrict t83TBLqMpNhuwQR6km9U9WSkjyTw8GN9ckBt3tiW0Kim5TYePoYaLpaoGhDJxVf

