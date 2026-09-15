DROP INDEX IF EXISTS idx_schedule_imports_term;
ALTER TABLE schedule_imports
    DROP COLUMN IF EXISTS term_id,
    DROP COLUMN IF EXISTS file_sha256,
    DROP COLUMN IF EXISTS created_count,
    DROP COLUMN IF EXISTS skipped_count,
    DROP COLUMN IF EXISTS merged_count,
    DROP COLUMN IF EXISTS warning_count,
    DROP COLUMN IF EXISTS created_codes,
    DROP COLUMN IF EXISTS skipped_codes,
    DROP COLUMN IF EXISTS merged_codes,
    DROP COLUMN IF EXISTS warnings,
    DROP COLUMN IF EXISTS errors,
    DROP COLUMN IF EXISTS fatal_error,
    DROP COLUMN IF EXISTS started_at;
