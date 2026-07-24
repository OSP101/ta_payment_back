-- PDPA: the national ID is only needed transiently to render the creditor-form
-- PDF; it is no longer retained at rest (scrubbed right after the form is
-- generated — see DocsService.AttachGeneratedCreditorForm). Clear any values
-- already stored so existing rows comply immediately.
UPDATE ta_profiles SET national_id = NULL WHERE national_id IS NOT NULL;
UPDATE ta_profile_submissions SET national_id = NULL WHERE national_id IS NOT NULL;
