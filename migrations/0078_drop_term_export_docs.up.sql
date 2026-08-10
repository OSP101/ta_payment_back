-- 10/08/2026 — reverses 0075. Staff asked ปะหน้าจ่ายตรง to match the office's
-- own template exactly (docs/ปะหน้าจ่ายตรง-CY.xls): the memo number, its date,
-- and the ผู้แจ้งโอน signature are filled in by hand after printing, not
-- configured on a settings screen. See export_transfer_cover.go's
-- transferCoverBlankMemoLine for the static line that replaced this table's
-- per-term values.
DROP TABLE IF EXISTS term_export_docs;
