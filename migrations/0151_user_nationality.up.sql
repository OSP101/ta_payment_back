-- Foreign TAs (06/10/2026, revised the same day after asking the office).
-- The creditor form takes "เลขบัตรประจำตัวประชาชน/เลขประจำตัวผู้เสียภาษี";
-- the office's rule for a foreigner is simpler than the form's own note: they
-- write their PASSPORT number in that box and attach a copy of the passport in
-- place of the citizen-ID card. Nothing else. Nationality is chosen when the
-- account is created.
--
-- The number itself still lives in ta_profiles.citizen_id_enc: for a foreigner
-- it is the passport number, which the transfer cover also uses as their
-- PromptPay number.
ALTER TABLE users
  ADD COLUMN nationality TEXT NOT NULL DEFAULT 'thai'
    CHECK (nationality IN ('thai', 'foreign'));

-- The documents a TA must have approved before their profile is complete.
-- Every "ส่งครบ / อนุมัติครบ" question asks THIS, so the review queue, the
-- dashboard, the TA-request gate and announcements cannot disagree about it.
--   thai:    citizen-ID copy, bank book, creditor form
--   foreign: passport copy,   bank book, creditor form
CREATE FUNCTION ta_required_doc_kinds(uid UUID) RETURNS TEXT[]
  LANGUAGE sql STABLE AS $$
    SELECT CASE WHEN u.nationality = 'foreign'
                THEN ARRAY['passport','bank_book','creditor_form']
                ELSE ARRAY['national_id','bank_book','creditor_form'] END
      FROM users u WHERE u.id = uid
$$;
