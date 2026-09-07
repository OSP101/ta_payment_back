-- Only remove the seats this migration itself created: still-vacant,
-- still-inactive, one of these five titles. A seat staff has since assigned
-- someone to (or activated) has real data and is left alone.
DELETE FROM admin_officers
WHERE user_id IS NULL
  AND is_active = FALSE
  AND full_name = ''
  AND title IN (
    'คณบดีวิทยาลัยการคอมพิวเตอร์',
    'รองคณบดีฝ่ายวิชาการ',
    'รองคณบดีฝ่ายบริหาร',
    'รองคณบดีฝ่ายวิจัยและนวัตกรรม',
    'หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์'
  );
