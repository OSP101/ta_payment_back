-- Record THAT a national ID was provided, separately from the number itself.
--
-- AttachGeneratedCreditorForm scrubs ta_profiles.national_id to NULL once the
-- creditor-form PDF exists — correct under PDPA, the number is only needed to
-- render that PDF. But every "is step 1 complete?" check in the UI asked
-- "is national_id 13 digits?", so the scrub silently un-completed the step the
-- TA had just finished. The checklist could never reach 4/4, and the
-- onboarding nag never went away, no matter what the TA did.
--
-- Splitting the fact from the value fixes it without retaining anything: the
-- timestamp says the TA supplied a validated 13-digit number, and the number
-- is still gone.

ALTER TABLE ta_profiles
    ADD COLUMN IF NOT EXISTS national_id_provided_at TIMESTAMPTZ;

-- Backfill: completed_at is only ever set by UpsertProfile, which rejects a
-- national ID that is not 13 digits. So a row with completed_at is a row where
-- a valid number was supplied — whether or not it has since been scrubbed.
UPDATE ta_profiles
   SET national_id_provided_at = completed_at
 WHERE completed_at IS NOT NULL
   AND national_id_provided_at IS NULL;
