-- Restores the 0024 seed only; BOT-synced rows removed by the up migration are not recoverable.
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
ON CONFLICT (holiday_date, source, (COALESCE(start_time, TIME '00:00'))) DO NOTHING;
