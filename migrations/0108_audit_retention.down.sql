DROP INDEX IF EXISTS audit_archives_covers_idx;
DROP TABLE IF EXISTS audit_archives;

-- Back to the unconditional guard from 0107.
CREATE OR REPLACE FUNCTION audit_logs_append_only() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs is append-only: % is not permitted', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

DROP FUNCTION IF EXISTS audit_retention();
