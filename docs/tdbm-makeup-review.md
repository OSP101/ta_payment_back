# TDBM original-session review

The TDBM extra-teachings feed supplies the replacement date, times, and
section, but does not identify the original cancelled sitting. Section matching
and original-session matching are separate states.

`GET /tdbm/extra-teachings?academic_year=2569&semester=2` now includes:

- `matched_section_id`: the section resolved in TA PAY.
- `original_kind`: `lecture` or `lab` when applied.
- `match_method`: `manual`, `automatic`, or an empty string.
- `resolution_reason`: why the original sitting still needs attention.
- `match_options`: outstanding holiday periods for that section, including
  original date, kind, times, holiday name, and whether sent hours block it.

Staff and administrators can submit up to 100 explicit matches together:

```http
POST /tdbm/match-makeups
Content-Type: application/json

{
  "academic_year": 2569,
  "semester": 2,
  "items": [
    {"extra_class_id": 123, "original_date": "2026-11-24", "kind": "lecture"},
    {"extra_class_id": 124, "original_date": "2026-11-24", "kind": "lab"}
  ]
}
```

The two entries may have different replacement dates. Each entry replaces
one original period; splitting a single original period across multiple TDBM
entries requires a separate scheduling-model change and is not inferred here.
The response is `{"matched": 2}`. Every selection is validated again under
section locks. An unavailable period, cancelled/consumed entry, wrong term,
export lock, or sent original hours refuses the entire batch. Both dates must
be in the course window. Existing holiday-overlap policy continues to trust
the actual replacement sitting supplied by TDBM.

Makeup creation, removal of unsent drafts for the cancelled sitting, TDBM alias
links, and actor-attributed audit records commit in the same transaction.
Existing aliases of a single sitting are consumed together.

Automatic matching continues only for a section with one outstanding holiday
date group and one replacement date group of equal period counts. It no longer
guesses between multiple dates chronologically or treats equal original and
replacement dates as proof. Existing links are preserved and labelled for
staff inspection, not silently rewritten.

## Upstream API follow-up

The durable solution is for TDBM to return an explicit original sitting:
`original_class_date`, `original_period_kind` (or a stable period ID), plus the
section identity and a stable submission ID. Multi-part rescheduling also
needs the original duration and an explicit relationship between replacement
parts, rather than clients assuming that several entries belong together.
No changes to the external TDBM service are made by this feature.

## Verification

Run the `TestTDBM` service tests with `TEST_DATABASE_URL` pointing to an
isolated PostgreSQL instance. The frontend provides
`npm run test:tdbm-review:browser`; set `TDBM_TEST_BASE_URL` when reusing an
existing local dev server. Its API data and writes are intercepted fixtures.
