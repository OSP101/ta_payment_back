-- 15/09/2026 — flesh out the registrar-import ledger (TOR §3.3 ข.5).
--
-- schedule_imports has existed since 0001_init, and CommitImport has always
-- written one row per successful commit — but only aggregate counts
-- (row_count, error_count, a `summary` jsonb of counts) and no read path at
-- all: no handler, no endpoint, no screen. Found during the TOR acceptance
-- review. This adds what staff actually need to answer "what happened last
-- time I imported this file": which term, which codes were created/skipped/
-- merged, the warning and error MESSAGES (not just counts), and a hash to
-- recognise "this is the same file I already tried".
--
-- term_id is nullable only because it cannot be backfilled for any row that
-- predates this column (the old summary jsonb never carried it) — every row
-- written from here on always sets it.
ALTER TABLE schedule_imports
    ADD COLUMN IF NOT EXISTS term_id       UUID REFERENCES academic_terms(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS file_sha256   TEXT,
    ADD COLUMN IF NOT EXISTS created_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS skipped_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS merged_count  INT NOT NULL DEFAULT 0,
    -- warning_count/warnings: rows a course-with-no-timetable shape is
    -- EXPECTED to produce (โครงงาน/สหกิจ/วิทยานิพนธ์) — split from
    -- error_count/errors 15/09/2026 for the same reason ImportResult was
    -- split (see parseNormalizedSheet's doc comment): before the split, a
    -- clean 127/127 import of the real registrar file reported "error 65
    -- รายการ" in this very ledger.
    ADD COLUMN IF NOT EXISTS warning_count INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS created_codes TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS skipped_codes TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS merged_codes  TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS warnings      TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS errors        TEXT[] NOT NULL DEFAULT '{}',
    -- Set only when the whole commit failed outright (the file itself could
    -- not be parsed) — before this, such an attempt left NO row at all, so a
    -- staff member who uploaded a corrupt file had no record they had tried.
    ADD COLUMN IF NOT EXISTS fatal_error   TEXT,
    ADD COLUMN IF NOT EXISTS started_at    TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_schedule_imports_term ON schedule_imports (term_id, at DESC);
