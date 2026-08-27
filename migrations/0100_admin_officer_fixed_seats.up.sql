-- Two changes, both from the same decision: admin_officers is now a FIXED
-- roster of seats (staff reassign who holds a seat; they no longer add or
-- delete seats from the settings screen), and holding an active seat is what
-- grants the executive dashboard — not a separately-ticked flag.

-- users.is_executive is superseded by "has an active admin_officers row" (see
-- AdminOfficerService, rbac.RBAC.Roles, RequireExecutiveView, AccountGuard —
-- all now derive it live via EXISTS instead of reading this column). Carry
-- forward anyone currently flagged so they don't lose access silently: if
-- they don't already hold a seat, give them a placeholder one under their
-- existing admin_position text (or a generic title when that's blank too).
INSERT INTO admin_officers (id, user_id, academic_prefix, full_name, title, is_active)
SELECT gen_random_uuid(), u.id, '', u.first_name || ' ' || u.last_name,
       COALESCE(NULLIF(u.admin_position, ''), 'ผู้บริหาร (ย้ายมาจากสิทธิ์เดิม)'),
       TRUE
FROM users u
WHERE u.is_executive
  AND u.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_officers ao WHERE ao.user_id = u.id AND ao.is_active);

ALTER TABLE users DROP COLUMN IF EXISTS is_executive;
