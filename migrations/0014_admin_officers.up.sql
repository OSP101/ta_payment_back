-- Executive / administrative officers whose names + titles appear on
-- generated official documents (e.g., dean, vice deans, head of department,
-- finance certifier). Staff maintain this list from the settings page.
--
-- Each row is a single officer with:
--   academic_prefix — vocative academic title, e.g., "ผศ.ดร.", "รศ."
--   full_name       — ชื่อ-นามสกุล (without prefix)
--   title           — administrative title/position, e.g., "คณบดีคณะวิศวกรรมศาสตร์"
--   sort_order      — display order (staff reorder to control document layout)
--   is_active       — soft-hide without deleting historical references

CREATE TABLE IF NOT EXISTS admin_officers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    academic_prefix TEXT NOT NULL DEFAULT '',
    full_name       TEXT NOT NULL,
    title           TEXT NOT NULL,
    sort_order      INT  NOT NULL DEFAULT 0,
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS admin_officers_sort_idx
    ON admin_officers (sort_order, created_at);
