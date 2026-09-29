-- Who sent a work_log back. The TA screen said "อาจารย์ส่งบันทึกเวลากลับให้แก้ไข"
-- for every rejected row, including the ones a staff officer sent back from
-- the payout review, because nothing recorded which side it came from.
-- NULL on rows rejected before this column existed (read as the lecturer, who
-- sent back almost all of them).
ALTER TABLE work_logs ADD COLUMN IF NOT EXISTS rejected_by_role TEXT
    CHECK (rejected_by_role IN ('lecturer', 'staff'));
