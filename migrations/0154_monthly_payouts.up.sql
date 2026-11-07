-- Snapshot the pre-monthly document coverage ONCE. New exports must not move
-- the cutover or reinterpret a folder finance already received as monthly.
CREATE TABLE payout_legacy_months (
 term_id UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
 year_month TEXT NOT NULL CHECK (year_month ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
 PRIMARY KEY(term_id, year_month)
);
INSERT INTO payout_legacy_months
SELECT DISTINCT tc.term_id, m.ym
FROM export_batches eb JOIN teaching_courses tc ON tc.id=eb.teaching_course_id
CROSS JOIN LATERAL unnest(eb.months) m(ym)
WHERE m.ym ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'
ON CONFLICT DO NOTHING;
INSERT INTO payout_legacy_months
SELECT DISTINCT tc.term_id,
 ((left(sp.year_month,4)::int - 543 + CASE WHEN right(sp.year_month,2)::int <= 5 THEN 1 ELSE 0 END)::text || '-' || right(sp.year_month,2))
FROM export_batches eb JOIN teaching_courses tc ON tc.id=eb.teaching_course_id
JOIN submission_periods sp ON sp.term_id=tc.term_id
WHERE COALESCE(cardinality(eb.months),0)=0
ON CONFLICT DO NOTHING;

-- A previously issued graduate-only transfer cover is historical too.
INSERT INTO payout_legacy_months
 SELECT DISTINCT e.term_id,m.ym FROM transfer_cover_exports e
 CROSS JOIN LATERAL unnest(e.months) m(ym)
 WHERE m.ym ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'
 ON CONFLICT DO NOTHING;

-- Retain the old key values 1/2 for historical folders. YYYYMM identifies a
-- new monthly folder; the API also returns year_month and its display label.
ALTER TABLE document_progress DROP CONSTRAINT document_progress_fiscal_round_check;
ALTER TABLE document_progress ADD CONSTRAINT document_progress_fiscal_round_check
 CHECK (fiscal_round IN (1,2) OR (fiscal_round BETWEEN 190001 AND 999912 AND fiscal_round % 100 BETWEEN 1 AND 12));
ALTER TABLE signature_checklist DROP CONSTRAINT signature_checklist_fiscal_round_check;
ALTER TABLE signature_checklist ADD CONSTRAINT signature_checklist_fiscal_round_check
 CHECK (fiscal_round IN (1,2) OR (fiscal_round BETWEEN 190001 AND 999912 AND fiscal_round % 100 BETWEEN 1 AND 12));
COMMENT ON COLUMN document_progress.fiscal_round IS 'Document scope: legacy fiscal folder 1/2, or monthly Gregorian YYYYMM. Never share signatures across scopes.';

CREATE TABLE monthly_payout_batches (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 term_id UUID NOT NULL REFERENCES academic_terms(id),
 year_month TEXT NOT NULL CHECK (year_month ~ '^[0-9]{4}-(0[1-9]|1[0-2])$'),
 version INT NOT NULL CHECK(version > 0),
 file_path TEXT NOT NULL,
 file_name TEXT NOT NULL,
 course_ids UUID[] NOT NULL,
 total_baht NUMERIC(12,2) NOT NULL CHECK(total_baht >= 0),
 generated_by UUID REFERENCES users(id),
 generated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(term_id,year_month,version)
);

CREATE TABLE monthly_payout_signers (
 batch_id UUID NOT NULL REFERENCES monthly_payout_batches(id),
 teaching_course_id UUID NOT NULL REFERENCES teaching_courses(id),
 code TEXT NOT NULL, name_th TEXT NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('ta','lecturer','certifier')),
 signer_id UUID REFERENCES users(id), responsible TEXT NOT NULL
);
CREATE INDEX monthly_payout_signers_batch_idx ON monthly_payout_signers(batch_id);
