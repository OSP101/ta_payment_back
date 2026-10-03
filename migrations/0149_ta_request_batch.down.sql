DROP INDEX IF EXISTS ta_requests_batch_id_idx;
ALTER TABLE ta_requests DROP COLUMN IF EXISTS batch_id;
