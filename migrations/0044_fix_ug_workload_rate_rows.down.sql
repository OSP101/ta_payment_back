-- Reverses 0044. Restoring 200/250 re-introduces a course ceiling one third
-- below what the faculty funds, so this exists for schema rollback only.

UPDATE pay_rates
SET ug_workload_rate_regular = 200
WHERE ug_workload_rate_regular = 300;

UPDATE pay_rates
SET ug_workload_rate_special = 250
WHERE ug_workload_rate_special = 300;
