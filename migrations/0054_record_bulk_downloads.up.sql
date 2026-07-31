-- Record bulk downloads too, without letting them spend the quota.
--
-- THE BUG. Two rules were both right on their own and wrong together:
--
--   * the bulk download ("ดาวน์โหลดเอกสารทั้งหมด") is exempt from the per-TA
--     quota, because one click covers every approved TA and would otherwise
--     spend everyone's allowance at once;
--   * the "อย่าลืมดาวน์โหลด" reminder asks whether a TA's documents have been
--     downloaded, and it read that off this table — the quota counter.
--
-- So a bulk download wrote nothing here, and the reminder kept nagging about
-- files the officer had already saved. Worse than noise: the reminder exists
-- because the files are purged after 7 days, so an officer learning to ignore it
-- is how documents get lost.
--
-- THE FIX. This table now records every download, and a flag says which ones the
-- quota is counted against. The two questions were never the same question:
--
--   "how many copies have left the system?"  -> counts_toward_quota = TRUE
--   "have these files been handed off yet?"  -> any row at all
--
-- Recording bulk pulls here also puts them in the PII access trail beside the
-- individual ones, which is where someone auditing who touched a national ID
-- would look first — audit_logs alone required knowing that a separate
-- `ta_docs.download_all` action existed.

ALTER TABLE ta_doc_downloads
    ADD COLUMN counts_toward_quota BOOLEAN NOT NULL DEFAULT TRUE;

COMMENT ON COLUMN ta_doc_downloads.counts_toward_quota IS
    'FALSE for bulk (all-approved) downloads: recorded as an access event and as '
    'proof the documents were handed off, but exempt from maxDocDownloads.';

-- Partial index so the quota COUNT(*) does not have to read the bulk rows, which
-- will outnumber the individual ones (one per TA per bulk click).
CREATE INDEX ta_doc_downloads_quota_idx
    ON ta_doc_downloads (user_id)
    WHERE counts_toward_quota;

-- Backfill from the audit trail. Bulk downloads that already happened left no
-- row here, so without this the reminder keeps nagging about documents that were
-- downloaded before this migration — the exact symptom that prompted it.
--
-- audit_logs is the only record of those pulls. Each `ta_docs.download_all`
-- entry carries the TA ids inside `after->'ta_ids'`, the officer in actor_id and
-- the time in `at`, which is everything a row here needs.
--
-- actor_id is NOT NULL and references users(id), so entries whose officer has
-- since been deleted are skipped rather than losing the whole backfill; the audit
-- entry itself still records them. `round` falls back to 1 for a document set
-- that predates rounds, matching currentDocRound's own fallback.
INSERT INTO ta_doc_downloads (user_id, round, actor_id, downloaded_at, counts_toward_quota)
SELECT ta.id::uuid,
       COALESCE((SELECT MAX(d.round) FROM ta_documents d
                  WHERE d.user_id = ta.id::uuid AND d.superseded_at IS NULL), 1),
       a.actor_id,
       a.at,
       FALSE
  FROM audit_logs a
  CROSS JOIN LATERAL jsonb_array_elements_text(a.after -> 'ta_ids') AS ta(id)
 WHERE a.action = 'ta_docs.download_all'
   AND a.actor_id IS NOT NULL
   AND EXISTS (SELECT 1 FROM users u WHERE u.id = a.actor_id)
   AND EXISTS (SELECT 1 FROM users u WHERE u.id = ta.id::uuid);
