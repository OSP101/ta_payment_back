-- 15/08/2026 — when a slot was last actually used, so an idle one can be
-- safely handed to someone new instead of the sandbox just filling up
-- forever. See internal/demo/manager.go's Claim: when every slot is
-- claimed, it reclaims the least-recently-active one that's been idle past
-- config.DemoIdleReclaimDays rather than failing outright — this column is
-- what "least-recently-active" is measured against.
--
-- Distinct from last_reset_at (an explicit "เริ่มห้องทดลองใหม่" action) and
-- claimed_at (set once, at claim time): this updates on every login and
-- every reset, so someone who logs in occasionally without ever resetting
-- still reads as active, not idle.
ALTER TABLE demo_workspaces ADD COLUMN last_active_at TIMESTAMPTZ;

-- Existing rows (claimed before this column existed) start from claimed_at
-- rather than NULL — NULL would read as "never active, reclaim immediately"
-- for someone who may well still be using their slot.
UPDATE demo_workspaces SET last_active_at = COALESCE(last_reset_at, claimed_at) WHERE owner_email IS NOT NULL;
