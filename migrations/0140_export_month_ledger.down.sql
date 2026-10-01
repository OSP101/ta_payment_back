ALTER TABLE export_batches
    DROP COLUMN IF EXISTS previous_batch_id,
    DROP COLUMN IF EXISTS version;
DROP TABLE IF EXISTS export_month_ledger;
