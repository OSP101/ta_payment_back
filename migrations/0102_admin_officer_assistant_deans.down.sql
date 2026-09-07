DELETE FROM admin_officers
WHERE user_id IS NULL
  AND is_active = FALSE
  AND full_name = ''
  AND title IN (
    'ผู้ช่วยคณบดีฝ่ายแผนและประกันคุณภาพ',
    'ผู้ช่วยคณบดีฝ่ายดิจิทัล'
  );
