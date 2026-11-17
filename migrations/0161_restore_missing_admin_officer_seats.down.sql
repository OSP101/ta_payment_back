-- Intentionally preserve restored seats on rollback. They may already have
-- holders or document references, and cannot be distinguished reliably from
-- seats that existed before this repair. Do not delete operational data.
SELECT 1;
