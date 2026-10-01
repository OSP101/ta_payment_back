-- 01/10/2026: the ประชาสัมพันธ์ page shows what happened after a notice went
-- out — who opened it, and lets an officer nudge only those who have not.

-- When the recipient first opened the announcement itself (not the bell line).
-- NULL = not opened yet. Set once and never moved, so "อ่านแล้ว 27 จาก 42" only
-- ever goes up.
ALTER TABLE announcement_recipients
    ADD COLUMN IF NOT EXISTS read_at TIMESTAMPTZ;

-- When unread recipients were last reminded. One timestamp per announcement is
-- enough to stop the reminder button being pressed twice in a day.
ALTER TABLE announcements
    ADD COLUMN IF NOT EXISTS reminded_at TIMESTAMPTZ;
