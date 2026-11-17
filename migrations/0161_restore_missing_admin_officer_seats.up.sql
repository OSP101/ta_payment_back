-- Launch resets clear admin_officers but retain schema_migrations, so the
-- original roster migrations (0101/0102) cannot restore the missing seats.
-- Insert only absent titles, including when an existing seat is inactive or
-- unassigned. Never change existing IDs, holders, names, or activation state.
-- Vacant seats stay inactive until staff assigns a real holder.
INSERT INTO admin_officers (id, user_id, academic_prefix, full_name, title, is_active)
SELECT gen_random_uuid(), NULL, '', '', t, FALSE
FROM unnest(ARRAY[
    'คณบดีวิทยาลัยการคอมพิวเตอร์',
    'รองคณบดีฝ่ายวิชาการ',
    'รองคณบดีฝ่ายบริหาร',
    'รองคณบดีฝ่ายวิจัยและนวัตกรรม',
    'หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์',
    'ผู้ช่วยคณบดีฝ่ายแผนและประกันคุณภาพ',
    'ผู้ช่วยคณบดีฝ่ายดิจิทัล'
]) AS t
WHERE NOT EXISTS (SELECT 1 FROM admin_officers ao WHERE ao.title = t);
