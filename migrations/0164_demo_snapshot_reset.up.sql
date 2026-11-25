-- Only explicit sandbox resets/checkpoint restores may truncate fake history.
-- Production history and demo UPDATE/DELETE remain append-only.
CREATE OR REPLACE FUNCTION protect_request_calculation_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'TRUNCATE'
       AND TG_TABLE_SCHEMA ~ '^demo_slot_[0-9]+$'
       AND current_setting('ta_payment.demo_reset', true) = 'on' THEN
        RETURN NULL;
    END IF;
    RAISE EXCEPTION 'Calculation snapshots are append-only';
END;
$$;
