ALTER TABLE announcements DROP COLUMN IF EXISTS reminded_at;
ALTER TABLE announcement_recipients DROP COLUMN IF EXISTS read_at;
