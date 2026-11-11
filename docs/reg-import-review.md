# REG import review

Apply migration `0160_reg_review` before deploying the backend and frontend.
The supported source is the unmodified REG `sysTitle` export, either BIFF8
`.xls` or `.xlsx`. Existing `Normalized` `.xlsx` files can use the review UI
as well. File extension and container signature must agree. Limit: 20 MB.

## Workflow

1. POST multipart `file`, `term_id` to `/teaching-courses/import?dry_run=1`.
   The returned `review` contains editable courses/sections/timetables, immutable
   source cells, active lecturer candidates and structured issues. No writes occur.
2. Edit the `review` and submit it as an additional JSON multipart field on the
   same dry-run endpoint. Course identities and source-section identities cannot
   disappear; exclusion requires an explicit skip reason. Source evidence is
   always reconstructed from the uploaded file.
3. Correct all errors and explicitly acknowledge warning IDs. The UI revalidates
   automatically after an editing pause; manual retry remains available. The
   server returns `ready` and `validation_hash`.
4. Submit the same file and review with `confirmed: true` to the commit endpoint.
   The server regenerates validation and compares the hash, including the term,
   file, values, acknowledgements and existing-course snapshot. Frontend readiness
   is never trusted. Active lecturer accounts are checked again inside the write
   transaction.

All included creations, explicitly selected merges, import history and audit log
commit in one serializable transaction. An error rolls back the whole batch.
Existing courses default to skip and are not overwritten. Reimport after a lost
response therefore cannot silently duplicate a successful batch. A deliberate
merge into an existing course respects its export lock. A new merged course must
use the newest code era as its base so the correct timetable/credits survive.

## Staff assistance and batch edits

Previews expose stable issue `kind` values and source-derived
`unmatched_officer_names`. The UI explains possible spelling differences,
missing/inactive lecturer accounts and ambiguous names; it does not infer a
specific cause from a failed match. Staff can search by full name/email, open
user management separately and refresh active candidates without losing edits.
A complete exact unique match to active lecturer accounts, with the source's
primary-owner order unchanged, is marked `lecturer_match_verified` and produces
no `lecturer_match` warning. This is recomputed on every preview and commit,
never trusted from client metadata. Partial/ambiguous matches, unnamed extra
teachers (e.g. “และคณะ”), and manual changes to the matched account/order still
require review. No heuristic percentage is presented as certainty.
An empty lecturer picker is local draft state, never an empty UUID in a request.

Batch tools are limited to explicitly selected, currently visible, included new
courses. They preview old/new values before applying curricula, tracks, study
levels or lecturer additions. Existing courses and skipped courses are excluded.
Curriculum updates default to filling invalid/missing classifications only;
lecturer additions retain existing teachers unless explicit primary promotion
is selected. Batch acknowledgement selects warning categories from a fresh
validated review. Errors, unresolved REG names and unassigned-lecturer warnings
cannot be acknowledged in bulk. Every batch invalidates final confirmation and
requires server revalidation; it does not write to the database.

## Source semantics

- A source Section is identified by course, section number **and track**. Regular
  and special `SEC 01` are distinct. Same-track duplicates remain visible for
  correction instead of being coalesced.
- Each newline in Day and Time describes a meeting/room. `MW` expands into Monday
  and Wednesday; `S` aliases Saturday. These interpretations require review.
- Several rooms at the same time remain separate schedule records. Weekly-hour
  comparison counts simultaneous same-kind meetings once; exact duplicates are
  blocked. Student counts are taken once per section, never once per meeting.
- `ตรี (พิเศษ)` supplies the special track. Programme tokens supply a suggested
  primary curriculum (`CP-ITII` maps to IT). Multiple programme reservations remain
  intact in source evidence and require confirmation of the primary classification.
- The source count is capacity, not a claim that actual enrolment has been verified.
  Staff can correct it in review; actual registration counts can also be updated
  through the existing enrolment workflow. Courses over 3,000 seats require an
  explicit warning acknowledgement; the shared aggregate ceiling is 100,000 and
  the per-section ceiling remains 3,000.
- Pending day/time is allowed only with a recorded reason and acknowledgement.
  Pending sections are flagged even when some valid meetings already exist. They
  continue to block TA requests until staff completes the section timetable.
- Missing lecturers require either assignment to an active lecturer or an explicit
  unassigned reason. The first selected lecturer is the primary owner.
- Prerequisites, exam text, reserved groups, original cells, corrections, skips
  and acknowledgements are retained in `teaching_courses.reg_source` and the
  staff-only import history `review_data`. Course exam columns are not fabricated:
  operational exam windows remain on the academic term.

## Verification

Private REG files are not committed as fixtures. To check the original locally:

```sh
REG_IMPORT_FIXTURE=/absolute/path/to/reg.xls \
REG_PREVIEW_OUTPUT=/tmp/reg-preview.json \
TEST_DATABASE_URL=postgres://postgres@localhost:55439/postgres?sslmode=disable \
go test ./internal/service -run '^TestREG' -count=1 -v
```

The database URL must point to an isolated test server: the harness creates and
drops throwaway databases. The real-file test checks 159 courses, 465 sections,
168 special sections and the 7,543-seat general-education course. Synthetic tests
exercise cross-track identity, source binding, warning invalidation, pending
timetables and full rollback after a forced database failure.

Frontend: `REG_REVIEW_PREVIEW=/tmp/reg-preview.json REG_CHROME_CHANNEL=chrome
npm run test:reg-review:browser` uses the real modal with mocked HTTP responses
to verify desktop/mobile rendering, source expansion, review edits, revalidation,
confirmation and the multipart commit payload. Its temporary route is removed
after the run; private previews and screenshots are not committed.
