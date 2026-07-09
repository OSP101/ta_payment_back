-- Retro-remove sort_order from admin_officers for installs where 0014 was
-- applied with the sort_order column before the requirement was dropped.
-- No-op on installs where 0014's updated version created the table without it.

ALTER TABLE admin_officers DROP COLUMN IF EXISTS sort_order;
DROP INDEX IF EXISTS admin_officers_sort_idx;
