# Request workload rules and retained calculation inputs

New requests created by the application use `ta_requests.workload_rule_version = 2`.
For eligibility, sum each section's declared workload for the TA's level, including
sections taught simultaneously. Graduate declarations must remain within 10–12
hours/week; undergraduates still need a positive total and per-section caps.
Payable-time feasibility, worklogs, rates, estimates, and settlement keep their
existing shared-session billing rules. A declared workload total is not a promise
of payable hours. In particular, graduate special-track payment remains a lump sum.

Version 1 is the migration default for every existing request. Rechecks and adding
sections to an existing request retain that version. No migration recalculates
historical decisions, amounts, workloads, or worklogs. Old clients inserting requests
without a version also keep version 1 during a rolling deployment.

## Retained evidence

`ta_request_calculation_snapshots` stores immutable JSON inputs in the same
transaction as the operation. A failure to store evidence rolls the operation back.
The application records submission before clash handling, the initial decision,
and before/after workload edits and section additions. Workloads remain separate
per section; deduplication never overwrites the original declaration.

Each snapshot contains its schema version, workload rule version, billing rule
identifier, eligibility thresholds, request/decision state, assignments and co-taught
groups, every workload field and description, section schedules, course/term,
TA class schedules, and the pay-rate version in force at capture time. The original
submission snapshot remains unchanged if live schedules, rates, or workloads change.
Older requests have no invented historical snapshot: their first future edit records
a baseline of what is actually available at that time. Existing workload edit audit
entries also retain before/after values.

Inspect evidence using read-only SQL (parameter `$1` is the request UUID):

```sql
SELECT id, event, created_at, inputs
FROM ta_request_calculation_snapshots
WHERE request_id = $1
ORDER BY created_at, id;
```

For a future rule correction, export these inputs, implement a separate versioned
calculator, and compare its results with the recorded decision and original
worklogs before approving any adjustments. Do not overwrite evidence or change
historical request versions to simulate a different policy. This change provides
source evidence, not a staff-facing recalculation or payment-adjustment tool.
Actual historical payments must additionally be reconciled with original worklogs,
claim documents, and their existing audit trail. Continue normal database backups;
append-only guards do not replace backups or protect against a database owner.

## Deployment and rollback

Apply migration 0162, deploy the backend, then deploy the frontend. This avoids
showing the new form total while the old backend still uses counted-once eligibility.
No production deployment is included in this implementation.

Snapshot UPDATE, DELETE, and TRUNCATE are rejected by database triggers. Their
request FK prevents deleting a request that still has retained evidence. Migration
rollback refuses to erase snapshots or version-2 requests; use a reviewed forward
migration to correct rules while preserving history. Rolling application code back
also requires care: older code cannot interpret version-2 requests correctly.
