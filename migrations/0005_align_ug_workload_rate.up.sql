-- Align undergrad workload rate with the Excel formula (ชีต "2_59 ป.ตรี"):
--   monthly_pay = weekly_workload × (undergrad_share × ตรี_rate + graduate_share × บัณฑิต_rate)
--   default: 50% × 200 + 50% × 400 = 300
-- ต่างจากของเดิมที่ใช้ 200/250 ตาม track — ตอนนี้ ปกติ vs พิเศษ ต่างกันแค่ "จำนวน นศ."
-- rate เดียวกัน (effective rate) ตามสูตร Excel

ALTER TABLE pay_rates ALTER COLUMN ug_workload_rate_regular SET DEFAULT 300;
-- ug_workload_rate_special คงคอลัมน์ไว้เพื่อ backward-compat แต่ backend จะไม่ใช้แล้ว

-- Split student count by track per teaching_course. ตามสูตร Excel:
--   H21 = จำนวน นศ. ภาคปกติ  → คำนวณงบภาคปกติ
--   L21 = จำนวน นศ. ภาคพิเศษ → คำนวณงบภาคพิเศษ
ALTER TABLE teaching_courses ADD COLUMN IF NOT EXISTS num_students_regular INT NOT NULL DEFAULT 0;
ALTER TABLE teaching_courses ADD COLUMN IF NOT EXISTS num_students_special INT NOT NULL DEFAULT 0;

-- Migrate: ยก num_students เดิม → num_students_regular (สมมติทั้งหมดเป็นภาคปกติ)
UPDATE teaching_courses
SET num_students_regular = num_students
WHERE num_students_regular = 0 AND num_students > 0;
