ALTER TABLE users ADD COLUMN IF NOT EXISTS is_executive BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE users u
SET is_executive = TRUE
FROM admin_officers ao
WHERE ao.user_id = u.id AND ao.is_active;

-- The placeholder rows this migration's up.sql created for anyone who only
-- had the flag (no real seat) are left in place — deleting them would erase
-- a real (if generic-titled) roster entry that may since have been edited.
