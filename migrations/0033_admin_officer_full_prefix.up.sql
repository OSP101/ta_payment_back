-- Expand admin-officer academic prefixes from abbreviations to their full Thai
-- forms so the official appointment order (คำสั่งแต่งตั้งทีเอ) prints titles the
-- way the registrar template does ("รองศาสตราจารย์…" not "รศ."). "ดร." stays
-- abbreviated (it has no long form in practice). Custom/manual prefixes not in
-- this map are left untouched.
UPDATE admin_officers SET academic_prefix = CASE academic_prefix
    WHEN 'รศ.ดร.' THEN 'รองศาสตราจารย์ ดร.'
    WHEN 'ผศ.ดร.' THEN 'ผู้ช่วยศาสตราจารย์ ดร.'
    WHEN 'ศ.ดร.'  THEN 'ศาสตราจารย์ ดร.'
    WHEN 'รศ.'    THEN 'รองศาสตราจารย์'
    WHEN 'ผศ.'    THEN 'ผู้ช่วยศาสตราจารย์'
    WHEN 'ศ.'     THEN 'ศาสตราจารย์'
    ELSE academic_prefix
END
WHERE academic_prefix IN ('รศ.ดร.', 'ผศ.ดร.', 'ศ.ดร.', 'รศ.', 'ผศ.', 'ศ.');
