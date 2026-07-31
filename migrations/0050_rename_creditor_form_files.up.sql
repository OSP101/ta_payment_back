-- Rename generated creditor-form documents to the current filename convention.
--
--   old (BuildCreditorFormPDF, before this change): creditor_form_<ชื่อ>_<นามสกุล>.pdf
--   new (taFileStem):                               <รหัสนักศึกษา>_<ชื่อ>_<นามสกุล>.pdf
--
-- ta_documents.filename is a stored string, written once when the PDF was
-- generated. Changing the Go code therefore renamed nothing that already
-- existed: the officer's review list kept showing `creditor_form_…` beside
-- newly generated `653020123-4_…`, and single-document downloads served the old
-- name. This brings the existing rows into line.
--
-- ONLY rows whose filename is exactly what the old code would have produced are
-- touched. A TA may have hand-uploaded a file they happened to name
-- `creditor_form_something.pdf`, and a TA's own filename is not ours to
-- rewrite — reconstructing the old name from the users row and demanding an
-- exact match is what keeps this from guessing. Rows for a user whose name has
-- since changed no longer match and are left alone: a rename we cannot verify
-- is worse than an old name we can explain.
--
-- The two helpers below must stay in step with taFileStem /
-- sanitizeFilenamePart in internal/service/docs.go. They are duplicated rather
-- than shared because a migration cannot call Go; the parity is pinned by
-- TestMigration0050_MatchesGoFilenameHelper, which runs this very file and
-- compares the result against the Go function.
--
-- The helpers are created and dropped inside this migration so they never
-- become part of the schema.

CREATE FUNCTION mig0050_sanitize(s TEXT) RETURNS TEXT AS $$
    -- Whitespace becomes '_', path/reserved characters become '-'. Thai
    -- characters are deliberately untouched: the name exists to be read.
    SELECT translate(
             translate(BTRIM(COALESCE(s, '')), E' \t\n\r', '____'),
             '/\:*?"<>|', '---------')
$$ LANGUAGE sql IMMUTABLE;

CREATE FUNCTION mig0050_stem(student_id TEXT, first_name TEXT, last_name TEXT)
RETURNS TEXT AS $$
    -- Empty parts are dropped, not left as gaps: a TA with no student ID on
    -- record must not get a filename starting with '_'.
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
   SET filename = mig0050_stem(u.student_id, u.first_name, u.last_name) || '.pdf'
  FROM users u
 WHERE u.id = d.user_id
   AND d.kind = 'creditor_form'
   -- The old builder replaced spaces only (strings.ReplaceAll(fn+"_"+ln, " ", "_")),
   -- so REPLACE mirrors it exactly. Anything else was not written by that code.
   AND d.filename = 'creditor_form_'
                    || REPLACE(COALESCE(u.first_name, '') || '_' || COALESCE(u.last_name, ''), ' ', '_')
                    || '.pdf';

DROP FUNCTION mig0050_stem(TEXT, TEXT, TEXT);
DROP FUNCTION mig0050_sanitize(TEXT);
