-- 0021_drop_hour_caps.down.sql
-- Recreate hour_caps table (schema from 0001_init).
CREATE TABLE IF NOT EXISTS hour_caps (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    credits     INT NOT NULL UNIQUE,
    hours_cap   INT NOT NULL,
    note        TEXT
);
