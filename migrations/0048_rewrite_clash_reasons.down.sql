-- Irreversible by design: 0048 replaces free text with better free text and
-- keeps no copy of the old sentence. Restoring it would mean re-deriving a
-- message the code no longer produces anywhere.
--
-- Nothing depends on the old wording — state_reason is display text, never
-- parsed — so a no-op down is honest rather than lossy.

SELECT 1;
