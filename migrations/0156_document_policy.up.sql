CREATE TABLE document_policy (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    retention_days INTEGER NOT NULL DEFAULT 7 CHECK (retention_days BETWEEN 1 AND 3650),
    download_limit INTEGER NOT NULL DEFAULT 2 CHECK (download_limit BETWEEN 1 AND 1000),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL
);
INSERT INTO document_policy (id) VALUES (TRUE);

-- Pending sets must not expire while staff are still reviewing them.
UPDATE ta_documents d SET expires_at = NULL
WHERE d.superseded_at IS NULL AND d.file_deleted_at IS NULL
  AND d.kind = ANY(ta_required_doc_kinds(d.user_id))
  AND (SELECT COUNT(*) FROM ta_documents r WHERE r.user_id=d.user_id
       AND r.superseded_at IS NULL AND r.status='approved'
       AND r.kind = ANY(ta_required_doc_kinds(d.user_id)))
      < cardinality(ta_required_doc_kinds(d.user_id));

-- Align existing complete sets to the final document approval, preserving blobs
-- already deleted. Never base the clock on the TA's upload/submission time.
UPDATE ta_documents d SET expires_at = c.approved_at + INTERVAL '7 days'
FROM (
    SELECT user_id, MAX(reviewed_at) AS approved_at
    FROM ta_documents r
    WHERE superseded_at IS NULL AND status='approved'
      AND kind = ANY(ta_required_doc_kinds(user_id))
    GROUP BY user_id
    HAVING COUNT(*) = cardinality(ta_required_doc_kinds(user_id))
) c
WHERE d.user_id=c.user_id AND d.superseded_at IS NULL
  AND d.kind = ANY(ta_required_doc_kinds(d.user_id))
  AND d.file_deleted_at IS NULL AND c.approved_at IS NOT NULL;
