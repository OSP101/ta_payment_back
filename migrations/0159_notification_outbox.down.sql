-- DRAIN/CANCEL pending jobs and take an encrypted backup before deliberate rollback.
-- Application rollback can leave this additive table in place safely.
DROP TABLE notification_outbox;
