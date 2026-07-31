-- Phase 3 of the 24/07/2026 meeting plan: the appointment order (คำสั่งแต่งตั้ง)
-- becomes a sequence of rounds instead of a one-shot document.
--
-- Why: TA requests are never closed. A course whose TAs have not finished
-- filing their timetables simply is not ready when staff produce the order, so
-- it has to be SKIPPED and picked up later — and the later round must contain
-- only the stragglers, never the names already appointed. The lecturers were
-- explicit: "ไม่เอาวิชาที่ผ่านแล้วก่อนหน้ามาใส่อีก จะเอาแต่วิชาที่ล่าช้ามา".
--
-- Build() was completely stateless before this: it read every approved
-- assignment in the term and rendered them, with nothing recording that a
-- document had been issued. Running it twice produced the same names twice.

CREATE TABLE IF NOT EXISTS appointment_orders (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id       UUID NOT NULL REFERENCES academic_terms(id) ON DELETE CASCADE,
    -- Round 1 is the on-time batch; every later round is a late batch. Kept as
    -- a stored number rather than derived, because the order number printed on
    -- the paper document has to stay stable even if a round is deleted.
    round_no      INT  NOT NULL,
    order_no      TEXT NOT NULL,          -- "คำสั่งที่ 6/2569" as typed by staff
    order_date    TEXT NOT NULL,          -- Thai-formatted, as printed
    effective_date TEXT NOT NULL,
    signer_officer_id UUID REFERENCES admin_officers(id),
    ta_count      INT  NOT NULL DEFAULT 0,
    generated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    generated_by  UUID REFERENCES users(id),
    UNIQUE (term_id, round_no)
);

-- One row per (TA × course) that appeared on a printed order. Membership here
-- is what makes the next round skip them, so it is the real ledger — the PDF
-- itself is just a rendering of it.
CREATE TABLE IF NOT EXISTS appointment_order_items (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    appointment_order_id UUID NOT NULL REFERENCES appointment_orders(id) ON DELETE CASCADE,
    teaching_course_id UUID NOT NULL REFERENCES teaching_courses(id) ON DELETE CASCADE,
    ta_id              UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- A TA may legitimately assist the same course across two terms, so the
    -- pair is unique per ORDER, and the "already issued" lookup is scoped by
    -- the order's term.
    UNIQUE (appointment_order_id, teaching_course_id, ta_id)
);

CREATE INDEX IF NOT EXISTS appointment_order_items_pair_idx
    ON appointment_order_items (teaching_course_id, ta_id);

COMMENT ON TABLE appointment_orders IS
    'One printed คำสั่งแต่งตั้ง. round_no 1 = on time; 2+ = late rounds for courses that were not ready earlier.';
