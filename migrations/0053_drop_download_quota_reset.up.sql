-- Remove the admin quota-reset mechanism added in 0052.
--
-- The policy changed to "exhausted is exhausted": there is no way to hand back a
-- spent download allowance. With no reset, `reset_at` / `reset_by` can never be
-- written, and a column that is always NULL alongside a `reset_at IS NULL`
-- filter in three queries is worse than no column at all — it reads as a
-- feature that exists.
--
-- 0052 shipped and was applied, so it is undone with a forward migration rather
-- than by editing history: an environment that already has the columns must end
-- up in the same shape as one built from scratch.
--
-- Any reset already performed stays performed. Its rows keep reset_at, and
-- dropping the column makes them count again — correct under the new policy,
-- where those downloads were never forgivable in the first place.

DROP INDEX IF EXISTS ta_doc_downloads_active_idx;
CREATE INDEX ta_doc_downloads_user_round_idx
    ON ta_doc_downloads (user_id, round);

ALTER TABLE ta_doc_downloads
    DROP COLUMN IF EXISTS reset_by,
    DROP COLUMN IF EXISTS reset_at;
