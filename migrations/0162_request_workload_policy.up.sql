-- Existing requests retain the original rule, including future rechecks.
ALTER TABLE ta_requests ADD COLUMN workload_rule_version smallint NOT NULL DEFAULT 1
    CHECK (workload_rule_version IN (1, 2));

-- Immutable source snapshots. Do not backfill historical inputs we cannot know.
CREATE TABLE ta_request_calculation_snapshots (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id uuid NOT NULL REFERENCES ta_requests(id),
    actor_id uuid NOT NULL,
    event text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    inputs jsonb NOT NULL
);
CREATE INDEX ON ta_request_calculation_snapshots(request_id, created_at, id);
CREATE FUNCTION protect_request_calculation_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Calculation snapshots are append-only';
END;
$$;
CREATE TRIGGER request_calculation_snapshot_immutable
    BEFORE UPDATE OR DELETE ON ta_request_calculation_snapshots
    FOR EACH ROW EXECUTE FUNCTION protect_request_calculation_snapshot();
CREATE TRIGGER request_calculation_snapshot_no_truncate
    BEFORE TRUNCATE ON ta_request_calculation_snapshots
    FOR EACH STATEMENT EXECUTE FUNCTION protect_request_calculation_snapshot();
