-- Exactly one term may be the current one.
--
-- "ภาคเรียนปัจจุบัน" is a system-wide singleton: every screen that defaults a
-- term, every window that opens "this term", reads it. Nothing enforced that.
-- UpsertTerm wrote is_active straight through, so ticking the box on a new term
-- left the old one ticked too, and two terms both claimed to be current — which
-- of them won then depended on row order.
--
-- Demote everything but the newest active term before the index goes on, or
-- creating it would fail on exactly the databases that have the bug.
UPDATE academic_terms SET is_active = FALSE
WHERE is_active
  AND id <> (
    SELECT id FROM academic_terms
    WHERE is_active
    ORDER BY academic_year DESC, semester DESC
    LIMIT 1
  );

-- Indexing the boolean column itself, restricted to the true rows: every row in
-- the index carries the same value, so UNIQUE admits only one of them. Zero
-- active terms stays legal — a year can end before the next one is set up.
--
-- (academic_year, semester) already has its own UNIQUE constraint, so a term
-- cannot be duplicated within a year either.
CREATE UNIQUE INDEX ux_academic_terms_single_active
    ON academic_terms (is_active)
    WHERE is_active;
