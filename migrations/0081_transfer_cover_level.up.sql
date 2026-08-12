-- 12/08/2026 — ปะหน้าจ่ายตรงต้องแยกเป็นคนละไฟล์ระหว่าง ป.ตรี กับบัณฑิตศึกษา
-- (เจ้าหน้าที่ทำเบิกสองระดับไม่เหมือนกัน) ไม่ใช่แค่แยก sheet ในไฟล์เดียวเหมือน
-- เดิม แต่ละ generation ตอนนี้จึงมีระดับของตัวเอง
--
-- Nullable rather than NOT NULL DEFAULT: rows written before this column
-- existed covered BOTH levels in one file, and forcing them into either value
-- would misstate what was actually issued. NULL means "covered every level —
-- predates the split", distinguishable from a genuinely single-level file.
ALTER TABLE transfer_cover_exports
    ADD COLUMN IF NOT EXISTS level TEXT CHECK (level IN ('undergrad', 'graduate'));

COMMENT ON COLUMN transfer_cover_exports.level IS
    'Which level this generation covered: undergrad or graduate. NULL for rows predating the level split, which covered both in one file.';
