-- Move "term length in months" from pay_rates to academic_terms because each
-- term (regular / summer / special) can have a different length.
-- Default matches the previous global default (4). Budget/export code prefers
-- this per-term value and falls back to pay_rates.term_months if 0/NULL.
ALTER TABLE academic_terms ADD COLUMN IF NOT EXISTS months INT NOT NULL DEFAULT 4;
