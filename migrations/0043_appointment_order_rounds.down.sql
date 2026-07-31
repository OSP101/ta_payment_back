-- Reverses 0043. Dropping the ledger means the next Build() would re-issue
-- names that already appear on printed paper — acceptable only because the
-- down migration exists for local schema rollback, never for a live system
-- that has issued orders.

DROP INDEX IF EXISTS appointment_order_items_pair_idx;
DROP TABLE IF EXISTS appointment_order_items;
DROP TABLE IF EXISTS appointment_orders;
