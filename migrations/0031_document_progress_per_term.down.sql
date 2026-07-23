-- 0031_document_progress_per_term.down.sql
-- Restore the per-course table from 0030.
DROP TABLE IF EXISTS document_progress;

CREATE TABLE document_progress (
    teaching_course_id  UUID PRIMARY KEY REFERENCES teaching_courses(id) ON DELETE CASCADE,
    stage               INT  NOT NULL DEFAULT 0 CHECK (stage BETWEEN 0 AND 5),
    ta_signed_at        TIMESTAMPTZ,
    lecturer_signed_at  TIMESTAMPTZ,
    certifier_signed_at TIMESTAMPTZ,
    sent_finance_at     TIMESTAMPTZ,
    sent_treasury_at    TIMESTAMPTZ,
    note                TEXT,
    updated_by          UUID REFERENCES users(id),
    updated_by_name     TEXT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
