-- Refuse a rollback that would erase evidence or reinterpret new requests.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM ta_request_calculation_snapshots)
       OR EXISTS (SELECT 1 FROM ta_requests WHERE workload_rule_version = 2) THEN
        RAISE EXCEPTION 'Retained calculation history exists; use a forward migration';
    END IF;
END $$;
DROP TABLE ta_request_calculation_snapshots;
DROP FUNCTION protect_request_calculation_snapshot();
ALTER TABLE ta_requests DROP COLUMN workload_rule_version;
