-- 0030_document_progress.up.sql
-- Tracks the PHYSICAL document journey that happens OFF-system after a course
-- is exported: the wet-ink signature round and the routing to finance/treasury.
-- This is the "where are the documents right now?" board — the recurring source
-- of confusion. One row per teaching_course (each course's paperwork moves at
-- its own pace); staff advance the stage, everyone can read it.
--
--   stage 0 = ยังไม่เริ่ม
--   stage 1 = TA เซ็นครบ            (ta_signed_at)
--   stage 2 = อาจารย์ผู้สอนเซ็นครบ    (lecturer_signed_at)
--   stage 3 = ผู้รับรองเซ็นครบ         (certifier_signed_at)
--   stage 4 = ส่งการเงินแล้ว          (sent_finance_at)
--   stage 5 = การเงินส่งกองคลังแล้ว    (sent_treasury_at)

CREATE TABLE IF NOT EXISTS document_progress (
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
