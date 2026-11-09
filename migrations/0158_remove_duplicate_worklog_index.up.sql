-- Same keys/order as work_logs_assign_idx (0001). No business data changes.
-- lock_timeout bounds deployment lock acquisition; retry migration off peak.
SET LOCAL lock_timeout = '5s';
DROP INDEX IF EXISTS work_logs_assignment_date_idx;
