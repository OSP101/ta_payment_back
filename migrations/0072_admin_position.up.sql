-- Display-only administrative position (e.g. "หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์"),
-- separate from `title` (academic prefix: "ผศ.", "ดร.") and from `is_executive`
-- (dashboard access flag). A lecturer can hold a role like head of department
-- alongside their teaching role, and readers of the app need to see that,
-- but nothing in the system acts on it — it does not feed the appointment-order
-- or certifier flows, which draw from the separate `admin_officers` roster.
ALTER TABLE users ADD COLUMN IF NOT EXISTS admin_position TEXT;

COMMENT ON COLUMN users.admin_position IS
    'Free-text administrative position shown next to the role label (e.g. "หัวหน้าสาขาวิชา..."). Display-only — staff set it via user management; it has no effect on document generation.';
