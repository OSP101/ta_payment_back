-- 14/08/2026 — one named checkpoint per demo workspace, so staff can save a
-- point in the walkthrough and jump back to it to rehearse the same steps
-- again, instead of only being able to reset all the way to empty. See
-- docs/PLAN-demo-sandbox.md §5 and internal/demo's SaveCheckpoint/
-- RestoreCheckpoint. The checkpoint's actual DATA lives in its own
-- demo_slot_N_checkpoint schema (a full table-by-table copy, see
-- SaveCheckpoint) — this column is only presence/recency metadata for the
-- panel to show "checkpoint saved at ..." and to know whether "ย้อนกลับ" has
-- anything to restore yet.
ALTER TABLE demo_workspaces ADD COLUMN checkpoint_saved_at TIMESTAMPTZ;
