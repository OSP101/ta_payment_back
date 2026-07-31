-- PDPA: sensitive personal data is never stored in the database.
--
-- Policy (2026-07-29): the national ID, bank details and signature exist only
-- to be printed onto the creditor-form PDF. The form the TA fills in feeds that
-- PDF and nothing else — no table keeps a copy, not even a scrubbed-later one.
--
-- This finishes what migration 0046 started. 0046 kept the *fact* that a
-- national ID had been supplied because the number itself was already being
-- scrubbed after the PDF was generated. With every sensitive column now gone,
-- there is nothing left to scrub and nothing to flag: "step 1 is complete" is
-- answered by "an approved creditor_form document exists", which is the
-- artefact that actually carries the data.
--
-- What replaced each reader:
--   * creditor-form PDF   — rendered from the request payload, never re-read
--   * monthly report PDF  — national-ID line and TA signature block removed
--   * finance export      — bank/ID columns removed; staff print the creditor
--                           form and send it alongside
--   * export readiness    — now checks for an approved creditor_form document
--                           instead of non-empty bank columns
--   * staff user admin    — bank fields removed from view and edit
--
-- prefix stays: คำนำหน้า is not sensitive, and keeping it lets the form
-- pre-fill without asking again.

ALTER TABLE ta_profiles
    DROP COLUMN IF EXISTS national_id,
    DROP COLUMN IF EXISTS national_id_provided_at,
    DROP COLUMN IF EXISTS bank_name,
    DROP COLUMN IF EXISTS bank_branch,
    DROP COLUMN IF EXISTS branch_code,
    DROP COLUMN IF EXISTS account_no,
    DROP COLUMN IF EXISTS account_name,
    DROP COLUMN IF EXISTS signature_svg,
    DROP COLUMN IF EXISTS signature_png_b64;

-- The per-round snapshots duplicated every one of those values. Their only
-- reader is the staff history panel, which needs the round/status/dates to
-- answer "how many times was this sent back and why" — not the data itself.
ALTER TABLE ta_profile_submissions
    DROP COLUMN IF EXISTS national_id,
    DROP COLUMN IF EXISTS bank_name,
    DROP COLUMN IF EXISTS bank_branch,
    DROP COLUMN IF EXISTS branch_code,
    DROP COLUMN IF EXISTS account_no,
    DROP COLUMN IF EXISTS account_name,
    DROP COLUMN IF EXISTS signature_svg;
