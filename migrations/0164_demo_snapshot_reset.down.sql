CREATE OR REPLACE FUNCTION protect_request_calculation_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Calculation snapshots are append-only';
END;
$$;
