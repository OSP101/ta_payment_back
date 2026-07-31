-- Restore the old creditor-form filename convention.
--
-- Cleanly invertible, unlike most data migrations: both names are derived from
-- the same users row, so the old one can be rebuilt exactly rather than
-- guessed. Rows matching the NEW name are moved back to the OLD one — which
-- also covers files generated after 0050 was applied, and that is correct: a
-- rollback puts the old code back, and the old code produces the old name.

CREATE FUNCTION mig0050_sanitize(s TEXT) RETURNS TEXT AS $$
    SELECT translate(
             translate(BTRIM(COALESCE(s, '')), E' \t\n\r', '____'),
             '/\:*?"<>|', '---------')
$$ LANGUAGE sql IMMUTABLE;

CREATE FUNCTION mig0050_stem(student_id TEXT, first_name TEXT, last_name TEXT)
RETURNS TEXT AS $$
    SELECT COALESCE(
             NULLIF(
               array_to_string(
                 array_remove(ARRAY[
                   mig0050_sanitize(student_id),
                   mig0050_sanitize(first_name),
                   mig0050_sanitize(last_name)
                 ], ''),
                 '_'),
               ''),
             'ta_document')
$$ LANGUAGE sql IMMUTABLE;

UPDATE ta_documents d
   SET filename = 'creditor_form_'
                  || REPLACE(COALESCE(u.first_name, '') || '_' || COALESCE(u.last_name, ''), ' ', '_')
                  || '.pdf'
  FROM users u
 WHERE u.id = d.user_id
   AND d.kind = 'creditor_form'
   AND d.filename = mig0050_stem(u.student_id, u.first_name, u.last_name) || '.pdf';

DROP FUNCTION mig0050_stem(TEXT, TEXT, TEXT);
DROP FUNCTION mig0050_sanitize(TEXT);
