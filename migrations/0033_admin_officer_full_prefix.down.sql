UPDATE admin_officers SET academic_prefix = CASE academic_prefix
    WHEN 'รองศาสตราจารย์ ดร.'   THEN 'รศ.ดร.'
    WHEN 'ผู้ช่วยศาสตราจารย์ ดร.' THEN 'ผศ.ดร.'
    WHEN 'ศาสตราจารย์ ดร.'      THEN 'ศ.ดร.'
    WHEN 'รองศาสตราจารย์'       THEN 'รศ.'
    WHEN 'ผู้ช่วยศาสตราจารย์'    THEN 'ผศ.'
    WHEN 'ศาสตราจารย์'          THEN 'ศ.'
    ELSE academic_prefix
END
WHERE academic_prefix IN ('รองศาสตราจารย์ ดร.', 'ผู้ช่วยศาสตราจารย์ ดร.', 'ศาสตราจารย์ ดร.', 'รองศาสตราจารย์', 'ผู้ช่วยศาสตราจารย์', 'ศาสตราจารย์');
