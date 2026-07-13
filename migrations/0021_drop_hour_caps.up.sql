-- 0021_drop_hour_caps.up.sql
-- Removes the per-credit hour cap table. Staff decision: cap ชั่วโมงต่อหน่วยกิต
-- ไม่จำเป็น — daily hour caps ตาม ประกาศ 731/2565 (ug_regular_daily_hour_cap
-- ฯลฯ ใน pay_rates) เป็นตัวคุมชั่วโมงแล้ว
DROP TABLE IF EXISTS hour_caps CASCADE;
