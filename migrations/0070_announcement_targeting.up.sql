-- 06/08/2026: announcements get a real audience, not just a role list.
--
-- "ประกาศถึงผู้ช่วยสอน" was the finest cut available, so a notice meant for the
-- eight TAs who have not filed their documents went to all of them — and the
-- other people learn to ignore announcements. Staff asked to aim at: everyone,
-- a role, one course, named people, or a condition ("ยังไม่ส่งเอกสาร",
-- "ยังไม่ทำตารางเรียน"), down to a single person.

-- ---------------------------------------------------------------------------
-- 1. The rule
-- ---------------------------------------------------------------------------
-- Kept as plain arrays rather than JSON so Postgres can type-check the ids and
-- the foreign keys still mean something. `audience` (role_code[]) stays as the
-- role part of the rule — every existing row keeps working unchanged.
ALTER TABLE announcements
    -- Courses: everyone attached to them (the lecturers and the appointed TAs).
    ADD COLUMN IF NOT EXISTS target_course_ids UUID[] NOT NULL DEFAULT '{}',
    -- Named people, for "ส่งถึงคนเดียว".
    ADD COLUMN IF NOT EXISTS target_user_ids UUID[] NOT NULL DEFAULT '{}',
    -- Narrowing conditions, ANDed onto the base selection. Values are checked
    -- in Go against a closed set (announceFilters) because each one is a query,
    -- not data — a name with no query behind it would silently match nobody.
    ADD COLUMN IF NOT EXISTS target_filters TEXT[] NOT NULL DEFAULT '{}',
    -- The term a course/condition filter is evaluated against. NULL = the
    -- active term at publish time; frozen here so a re-send next semester does
    -- not quietly retarget a different set of people.
    ADD COLUMN IF NOT EXISTS target_term_id UUID REFERENCES academic_terms(id);

COMMENT ON COLUMN announcements.target_filters IS
    'Narrowing conditions ANDed onto the role/course/person selection, e.g. ta_missing_documents, ta_missing_schedule. Closed set enforced in the service.';

-- ---------------------------------------------------------------------------
-- 2. The resolved audience
-- ---------------------------------------------------------------------------
-- announcement_recipients was the extra-email list; it becomes the materialised
-- audience — one row per person the announcement is actually for.
--
-- Materialising at publish time is what makes the FEED agree with the email:
-- both read this table, so "who can see this" and "who was mailed" can never
-- drift apart. It also freezes the audience, so someone who files their
-- documents tomorrow does not silently lose an announcement they already got.
-- The feed asks "is this person on this announcement" on every list request.
CREATE INDEX IF NOT EXISTS announcement_recipients_user_idx
    ON announcement_recipients (user_id) WHERE user_id IS NOT NULL;
