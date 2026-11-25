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
blocked. The source course and its Sections must have no operational
references; only base teacher/Section/timetable/exam rows can be transferred.

The main course may retain approved TA requests, assignments, request counts
and declared workloads, provided no subsequent work has started. Pending
requests must be decided first. Any work log (including drafts), TA duty
schedule, work-log correction history, reviewed/exported monthly workflow,
payout or unknown operational reference blocks the merge. Automatically
maintained budget-alert rows and untouched pending monthly rows do not count
as submitted work. Rejected/cancelled request history is preserved, and is
still checked for downstream work.

The approved main course, requests, target Sections, assignments and workload
IDs are unchanged. The merge does not add existing TAs to Sections retained as
separate groups. Before/after calculation snapshots record the original and
merged student counts under `before_existing_course_merge` and
`existing_course_merged`. The approval graph is part of the preview token and
audit evidence. Request/assignment/workload/monthly rows are locked during
inspection and commit, so a new time entry cannot race past the final checks.
The FK guard also checks the approval graph for future downstream tables;
records are never deleted to get past these guards.

Verification:

- `TEST_DATABASE_URL=... go test ./internal/service -run '^TestExistingCourseMerge' -count=1`
  includes approved-main preservation, pending monthly/budget-cache rows,
  draft/submitted/approved time, duty schedules, correction history, payment
  ledgers, monthly review, future references, stale workloads and time created
  after preview. It compares the request/assignment/workload graph before and
  after, checks calculation snapshots, repeated pre-time merges and source
  request rejection. A concurrency test observes the actual PostgreSQL lock
  wait for a new work-log insert while merge inspection holds its locks.
- The production-page scenario suite also seeds approved main-course requests
  on desktop, mobile Chrome and WebKit/iPhone, two tabs, stale target data,
  existing draft/submitted time and a reviewed month. An additional three-
  Section case matches the 120-student example (85 regular / 35 special after
  merge). SQL assertions check that approvals, assignments and workloads are
  byte-for-byte unchanged, with exactly two calculation snapshots per merged
  approved request and none for blocked operations.

- Reinspection must refresh `X-Edit-Views` for exactly the target and source
  course, even though it is a POST. Skipping all edit tracking for previews
  leaves old exact snapshots from the staff page's course/budget reads in
  memory: a preview can return 200 with no blockers while every commit still
  returns 409 after another edit. Preview reads now advance those two snapshots
  without consuming command receipts or refreshing unrelated courses.
- `TEST_DATABASE_URL=... go test ./internal/handler -run '^TestExistingMergeReinspection' -count=1 -v`
  verifies recovery after concurrent target changes and missing client
  snapshots. A successful preview must return only the two reviewed course
  snapshots; stale commits are still rejected before reinspection.
- `RUN_MERGE_PRODUCTION=1 TEST_DATABASE_URL=... go test ./internal/handler -run '^TestExistingMergeProductionPageScenarios$' -count=1 -v -timeout 35m`
  builds the optimized standalone app with `npm run build`, then enters the
  actual `/staff/teaching?term=...` page, including the staff layout, Beta
  acknowledgement, term provider, SWR cache and dynamically imported modal.
  It uses real JWT/session checks, router, services and disposable PostgreSQL
  data, without intercepting API responses or creating substitute React pages.
  Nineteen scenarios cover desktop Chrome, mobile Chrome, WebKit/iPhone,
  target/source changes, two tabs, linked source requests, reversed main course,
  incompatible credits, exported courses and the approved-main cases above.
  Database assertions verify 45/35 students (85/35 for the extra-Section case),
  preserved approval graphs and unchanged rows for blocked cases.
  The browser suite rejects unexpected API 404/5xx responses as well as page
  errors. After a merge, the staff page refreshes lists and retained courses
  without refetching the removed source course or its budget.
  Setup heartbeats keep the disposable session active during slow builds.
  A cached rerun can set `MERGE_REUSE_PRODUCTION=1`; the test verifies the
  frontend revision/diff and reuses the recorded loopback API address.
  On 2026-11-24, the production-page suite reproduced a persistent 409 after
  target changes (9/10 scenarios passed before the fix); all 10 passed after
  preview snapshots were refreshed. This is a local test environment result,
  not a production deployment verification.

- A term-filtered course list must return both its term edit snapshot and
  `course:*`. Returning only the term snapshot makes a subsequent merge commit
  fail with HTTP 428 even when its preview token is valid. Single-course reads
  must not refresh the collection snapshot. Preview POSTs remain repeatable;
  `commit=1` retains concurrency, stale-view and duplicate-command checks.
- `TEST_DATABASE_URL=... go test ./internal/handler -run 'Test(TeachingCourseListSnapshot|ExistingMergeFullRouter)' -count=1 -v`
  exercises the production router with real JWT/session authentication,
  account/role guards, edit middleware and PostgreSQL. It repeats previews with
  the same intent and verifies commit results (45 regular / 35 special), source
  removal and duplicate commit rejection.
- `RUN_MERGE_BROWSER=1 TEST_DATABASE_URL=... go test ./internal/handler -run '^TestExistingMergeBrowserIntegration$' -count=1 -v -timeout 10m`
  runs the real React modal against that API and throwaway database, with no
  merge-response mocks. Desktop Chrome, mobile Chrome and WebKit/iPhone each
  select both tracks, inspect three times, tap the visible confirmation,
  commit and verify the refreshed course list. Requires frontend dependencies,
  Chrome, Playwright WebKit (`npx playwright install webkit`) and OpenSSL.
  The frontend fixture uses local HTTPS, preserving production CSP behavior.
  Test credentials and temporary routes/certificates are removed afterward.

- `TEST_DATABASE_URL=... go test ./internal/service -run '^TestExistingCourseMerge' -count=1`
  uses an isolated throwaway database. Tests cover sums, separate-group identity,
  source evidence, teacher deduplication, stale data, operational references,
  export locks, invalid Section mappings, authorization and forced rollback.
- `npm run test:merge-existing:browser` uses mocked HTTP with real components
  on desktop and mobile, including blocked previews, changed choices and stale
  commit retry. Temporary test routes are removed automatically.
