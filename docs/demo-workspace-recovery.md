# Demo workspace initialization and recovery

A fresh `/api/demo/enter` used to commit workspace ownership before truncating
and seeding its tables. Migration 0162 added an immutable calculation-snapshot
table whose TRUNCATE trigger rejected that initialization. A retry resumed the
claimed, empty room and offered accounts that did not exist.

Migration 0164 permits truncation only on `demo_slot_N` tables during an explicit,
transaction-local `ta_payment.demo_reset` operation. Production snapshots remain
append-only, as do updates/deletes to demo snapshots. Apply this as a forward
migration; do not edit an already-applied 0162 or disable triggers globally.

Claims now hold the registry row lock until truncation, account seeding and
ownership changes commit together. Failures roll everything back. Explicit room
resets use the same transactional seed. Re-entering a previously claimed room
with zero users repairs the legacy failure state; populated rooms are preserved.
No allowlist entries or production user accounts need to be removed or recreated.

Registry connections are reused across schemas. Seed statements use per-execution
parameter inference so enum type IDs from one room cannot leak into the next.

Checkpoint restore also uses the reset transaction. Derived edit versions and
request idempotency records are excluded, including from older checkpoints, so
restore-time triggers cannot collide with historical version rows. The generated
training term includes the start of the current month to accommodate the demo
courses and submission period when training starts late in a month.

Validation against an isolated PostgreSQL server:

```sh
TEST_DATABASE_URL='<isolated test admin URL>' go test ./internal/demo -count=1
TEST_DATABASE_URL='<isolated test admin URL>' go test ./internal/service -run '^TestWorkloadPolicy_UndergradRawInputsAreImmutable$' -count=1
```

`workspace_lifecycle_test.go` covers fresh claims, all nine picker logins, reset,
rollback after an injected seed failure, recovery of a claimed empty room,
preservation of populated rooms, restoration of real calculation snapshots,
old checkpoint compatibility, claims across multiple schemas on one connection,
and production snapshot protection.
