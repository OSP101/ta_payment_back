-- 0024 — Seed Thai national holidays.
--
-- Only fixed-date holidays are seeded here. Lunar (มาฆบูชา, วิสาขบูชา,
-- อาสาฬหบูชา, เข้าพรรษา) and any cabinet-announced substitution days must be
-- entered by staff via the admin UI once officially announced. Silently
-- guessing lunar dates == paying TAs on the wrong day, which is exactly the
-- failure mode this whole feature exists to prevent.
--
-- Seeding 2026 H2 (current term forward) + full 2027. Older/future years are
-- staff-driven.

INSERT INTO public_holidays (holiday_date, name_th, name_en, source, note) VALUES
    -- 2026 H2
    ('2026-08-12', 'วันแม่แห่งชาติ / วันเฉลิมพระชนมพรรษาสมเด็จพระบรมราชชนนีพันปีหลวง', 'H.M. Queen Sirikit''s Birthday / Mother''s Day', 'national', NULL),
    ('2026-10-13', 'วันคล้ายวันสวรรคต ร.9', 'H.M. King Bhumibol Adulyadej Memorial Day', 'national', NULL),
    ('2026-10-23', 'วันปิยมหาราช', 'Chulalongkorn Day', 'national', NULL),
    ('2026-12-05', 'วันพ่อแห่งชาติ / วันคล้ายวันพระราชสมภพ ร.9 / วันชาติ', 'H.M. King Bhumibol Adulyadej''s Birthday / National Day / Father''s Day', 'national', NULL),
    ('2026-12-10', 'วันรัฐธรรมนูญ', 'Constitution Day', 'national', NULL),
    ('2026-12-31', 'วันสิ้นปี', 'New Year''s Eve', 'national', NULL),
    -- 2027 fixed dates
    ('2027-01-01', 'วันขึ้นปีใหม่', 'New Year''s Day', 'national', NULL),
    ('2027-04-06', 'วันจักรี', 'Chakri Memorial Day', 'national', NULL),
    ('2027-04-13', 'วันสงกรานต์', 'Songkran Festival', 'national', NULL),
    ('2027-04-14', 'วันสงกรานต์', 'Songkran Festival', 'national', NULL),
    ('2027-04-15', 'วันสงกรานต์', 'Songkran Festival', 'national', NULL),
    ('2027-05-01', 'วันแรงงานแห่งชาติ', 'National Labour Day', 'national', NULL),
    ('2027-05-04', 'วันฉัตรมงคล', 'Coronation Day', 'national', NULL),
    ('2027-06-03', 'วันเฉลิมพระชนมพรรษาสมเด็จพระราชินี', 'H.M. Queen Suthida''s Birthday', 'national', NULL),
    ('2027-07-28', 'วันเฉลิมพระชนมพรรษา ร.10', 'H.M. King Vajiralongkorn''s Birthday', 'national', NULL),
    ('2027-08-12', 'วันแม่แห่งชาติ / วันเฉลิมพระชนมพรรษาสมเด็จพระบรมราชชนนีพันปีหลวง', 'H.M. Queen Sirikit''s Birthday / Mother''s Day', 'national', NULL),
    ('2027-10-13', 'วันคล้ายวันสวรรคต ร.9', 'H.M. King Bhumibol Adulyadej Memorial Day', 'national', NULL),
    ('2027-10-23', 'วันปิยมหาราช', 'Chulalongkorn Day', 'national', NULL),
    ('2027-12-05', 'วันพ่อแห่งชาติ / วันคล้ายวันพระราชสมภพ ร.9 / วันชาติ', 'H.M. King Bhumibol Adulyadej''s Birthday / National Day / Father''s Day', 'national', NULL),
    ('2027-12-10', 'วันรัฐธรรมนูญ', 'Constitution Day', 'national', NULL),
    ('2027-12-31', 'วันสิ้นปี', 'New Year''s Eve', 'national', NULL)
ON CONFLICT (holiday_date, source) DO NOTHING;
