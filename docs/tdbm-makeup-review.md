# TDBM MariaDB synchronization

Since 2026-11-26, TA PAY reads TDBM directly from MariaDB. There are no
outbound TDBM HTTP API calls and no HTTP fallback. The previous API proposal
in the workspace docs is historical context, not the current integration.

## Connection and rollout

Apply migrations `0166_tdbm_mariadb` and `0169_tdbm_source_references` before running the new backend. Configure
`TDBM_DB_HOST`, `TDBM_DB_PORT`, `TDBM_DB_NAME`, `TDBM_DB_USER`,
`TDBM_DB_PASSWORD` and `TDBM_DB_TLS` in the private backend environment.
The discovered source schema is `cp_tdbm`; the password must not be committed.
The deployment runtime-env allowlist includes these settings and excludes the
retired `TDBM_API_BASE_URL`. Restart/redeploy is required to load the new code.

Each pull opens a bounded, read-only, repeatable-read transaction. It reads
`semesters`, `holidays`, `extra_teaching`, `teaching`, `classes`, `courses`,
`majors`, `teachers`, `status_histories`, and checks `extra_teaching_times`.
It never writes to the source database. Connection and driver errors are
sanitized before reaching staff or sync history.

## Triggers and snapshots

`POST /api/v1/tdbm-webhook` remains compatible with the upstream webhook.
Its body is a wake-up signal only. Manual sync and the hourly scheduler use
the same MariaDB source. Webhook bursts are coalesced, including a trailing
pull for changes arriving during an in-flight snapshot. A PostgreSQL advisory
transaction lock serializes full syncs across backend processes.

Each locally active/used term is read once per sync. The term comes from
`classes.semester_id -> semesters`, not the replacement date, which can differ
from the original sitting. Both holidays and makeups come from the same source
transaction. Missing/unreadable source terms are errors, never empty snapshots.
Complete snapshots detect history-only changes without relying on the parent
`updated_at`. Only successful complete snapshots mark missing entries; failed
reads leave the previous mirror intact. Empty successful snapshots also reconcile.

## Eligibility and original sittings

Only `opt_status IN ('A','D')` may create or link makeups. Pending `P`, cancelled
`C`, deleted `X`, empty and unknown codes remain stored but are not eligible.
Missing rows are retained with `source_missing=true`. Latest approval-history
status is stored separately; it does not override the agreed A/D rule.

Original date, times and kind come from
`extra_teaching.teaching_id -> teaching`, with matching source class identity.
`C` maps to lecture and `L` to lab. The mirror retains source IDs, original
sitting values, parent/history timestamps, and latest history status ID.
Duration is calculated from replacement start/end times, not the cached source
`duration`. Regular/special track comes from `majors.major_type` (`N`/`S`).

Exact source sittings support ordinary cancelled classes as well as holidays,
and multiple original/replacement days without chronological guesses. Rows for
courses not opened locally remain unmatched. Legacy staged rows without source
metadata retain conservative staff review until their first MariaDB snapshot.
The native `extra_teaching_times` table is currently empty; a snapshot using it
is rejected rather than silently applying only part of a replacement.

## Reconciliation and payment evidence

A changed, cancelled, deleted or missing source sitting updates/removes its
TDBM-owned local makeup transactionally. Aliases with the same original and
replacement share one sitting. Cancelling one alias cannot cancel another
eligible owner. Divergent replacements for one original are flagged for review.
Identical local makeups are adopted without changing their identity, notes or hours. Unlocked legacy and manually entered sitting dates/times follow the authoritative TDBM plan in place, with before/after audit evidence. Explicit no-makeup waivers remain protected. Related legacy links are reconciled as one atomic component, so a permutation of old API guesses can be repaired without collisions.

Reference rows are retained separately from usable makeups. As confirmed by the user on 2026-11-27, a genuine moved sitting takes precedence over an unchanged original-time row in the same teaching/class/document. A zero-duration row is superseded when that same document contains a positive-duration replacement, including a replacement restored to the original time. Raw source duration is a lifecycle marker here; payable hours still come from the current start/end times. Two different positive moved replacements remain a source conflict.

Section locks coordinate with staff matching. Submitted/approved hours and
exported course/month locks prevent automatic changes. The latest source row
is still stored, while `reconciliation_error` and the sync log explain the
blocked application. Re-sync retries after staff resolve the conflicting
hours or lock. Draft hours for the replaced/cancelled sitting are cleaned up
only in a successful transaction. Reconciliation changes are audited.

Partial reconciliation is reported as a warning with a count, not as a failed import. Genuine transport/database errors still report failures.

The staff list exposes `source_original_date`, `source_original_kind`, `source_reference`,
`source_missing` and `reconciliation_error`. Conflicts remain visible even
when a previous applied sitting is retained. Missing/conflicting entries have
no actionable review choices. Sync history can be filtered by academic year
and semester so an older-term pull does not replace the selected term's status.

## Verification

Run `go test ./internal/service -run TestTDBM -count=1` with an explicitly
configured isolated `TEST_DATABASE_URL`. This covers status eligibility,
original sittings, rescheduling, deletion/disappearance, aliases, retries,
financial evidence, manual rows, holiday removal and serialized term pulls.
`TDBM_TEST_LIVE=1` enables the read-only source snapshot test using explicit
`TDBM_DB_*` settings. The staff browser suite is
`npm run test:tdbm-review:browser`; its local API requests are intercepted
fixtures and do not change production data.
