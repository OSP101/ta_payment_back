-- The fixed list the settings screen has claimed to show since 0100
-- ("ตำแหน่งเป็นรายการคงที่ (คณบดี / รองคณบดี / หัวหน้าสาขา ฯลฯ)") never actually
-- existed as rows on a fresh install — a college with no one previously
-- flagged is_executive got an empty admin_officers table and the "ยังไม่มี
-- ข้อมูล..." empty state forever, with no way to add a seat (Upsert has no
-- create path by design). Seed the five seats so staff can assign holders
-- instead of the table needing to already contain someone.
--
-- Seeded is_active = FALSE (vacant): the same officer titles feed
-- signer_authority.go/certifier.go's "who signs" queries, which filter on
-- ao.is_active and treat a match as a resolved, real signer. A vacant seat
-- left active would be picked up as the dean/head with a blank name instead
-- of falling through to "no signer chosen" — inactive keeps that fallback
-- intact until staff actually assigns and switches a seat on.
INSERT INTO admin_officers (id, user_id, academic_prefix, full_name, title, is_active)
SELECT gen_random_uuid(), NULL, '', '', t, FALSE
FROM unnest(ARRAY[
    'คณบดีวิทยาลัยการคอมพิวเตอร์',
    'รองคณบดีฝ่ายวิชาการ',
    'รองคณบดีฝ่ายบริหาร',
    'รองคณบดีฝ่ายวิจัยและนวัตกรรม',
    'หัวหน้าสาขาวิชาวิทยาการคอมพิวเตอร์'
]) AS t
WHERE NOT EXISTS (SELECT 1 FROM admin_officers ao WHERE ao.title = t);
