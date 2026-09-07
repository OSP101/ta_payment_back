-- The fixed roster from 0101 covered dean + 3 vice deans but missed the two
-- assistant-dean seats the college's own board page lists (computing.kku.ac.th
-- /cp-board): ผู้ช่วยคณบดีฝ่ายแผนและประกันคุณภาพ and ผู้ช่วยคณบดีฝ่ายดิจิทัล.
-- Same vacant/inactive seeding as 0101, for the same reason (see that
-- migration's comment on why a seat starts inactive).
INSERT INTO admin_officers (id, user_id, academic_prefix, full_name, title, is_active)
SELECT gen_random_uuid(), NULL, '', '', t, FALSE
FROM unnest(ARRAY[
    'ผู้ช่วยคณบดีฝ่ายแผนและประกันคุณภาพ',
    'ผู้ช่วยคณบดีฝ่ายดิจิทัล'
]) AS t
WHERE NOT EXISTS (SELECT 1 FROM admin_officers ao WHERE ao.title = t);
