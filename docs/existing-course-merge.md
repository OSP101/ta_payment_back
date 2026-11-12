# Merge already-open courses

Staff use **รวมวิชาที่เปิดแล้ว** on `/staff/teaching` when offerings were
previously created separately. This is separate from `merge-code`, which adds
a registrar code that is not already open in the term.

POST `/teaching-courses/:target/merge-existing` with `source_id` and
`mappings: [{source: section_uuid, target: section_uuid_or_null}]` previews
both original courses, staff-selected Section destinations, blockers and a
snapshot `token`. Null means retain the source Section as a separate group.
Every source Section needs exactly one explicit choice; same-track folds only.

POST the same route with `?commit=1`, unchanged mappings, `token` and
`confirmed: true` to apply. A serializable transaction locks both courses in
UUID order and their Sections, regenerates the snapshot and validates all
blockers. Timetable and exam rows are part of the snapshot. A stale preview,
concurrent mutation or failed write cannot commit partial changes.

The chosen main course retains its identity, settings and primary teacher.
Other teachers are appended without duplicates; if no primary exists, the
source primary is retained. Section folds sum student counts and use the
chosen main Section timetable/exams, as explicitly described in the preview.
Kept-separate Sections retain their UUID, schedules and exams and receive a
source-code prefix. All source codes and aliases are retained on the main
course. Original records, including schedules/exams, remain in `reg_source`
and the transactional audit entry. The absorbed course is removed from the
active course list. Student aggregates are recomputed in the same transaction.

Allowed only within one term, with identical credits/hours/study level and
with the newer curriculum code selected as main. Exported offerings are
blocked. The FK guard blocks any operational references on either course or
its Sections, including future tables; only base teacher/Section/timetable/
exam rows are transferable here. Requests, assignments, payouts and related
operational work must be reviewed through their normal processes first; the
merge action never deletes them to get past this guard.

Verification:

- `TEST_DATABASE_URL=... go test ./internal/service -run '^TestExistingCourseMerge' -count=1`
  uses an isolated throwaway database. Tests cover sums, separate-group identity,
  source evidence, teacher deduplication, stale data, operational references,
  export locks, invalid Section mappings, authorization and forced rollback.
- `npm run test:merge-existing:browser` uses mocked HTTP with real components
  on desktop and mobile, including blocked previews, changed choices and stale
  commit retry. Temporary test routes are removed automatically.
