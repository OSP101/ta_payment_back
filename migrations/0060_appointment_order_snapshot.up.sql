-- Re-issuing a คำสั่ง must hand back the SAME document, not a fresh one.
--
-- The ledger records what an order contained (order number, dates, signer, and
-- the exact TA × course pairs) but every word that appears on the page —
-- course names, credit hours, student ids, TA honorifics, the signer's title —
-- was looked up live at render time. Rebuilding from the ledger would therefore
-- re-read all of it, and a TA who has since changed their name, or a course
-- whose credits were corrected, would come back on paper that is supposed to be
-- a copy of something already signed.
--
-- So the composed document is frozen here at generation time: the whole render
-- input, exactly as it was handed to the PDF and DOCX writers. A reprint
-- deserialises this and renders again, touching no other table.
--
-- Why the render INPUT rather than the rendered bytes: the two writers are
-- deterministic given identical input, the JSON is a fraction of the size, and
-- it stays readable — an officer asking "what did คำสั่ง 6/2569 actually say?"
-- can be answered with a query instead of a PDF viewer.
--
-- NULL means "issued before this column existed". Those orders cannot be
-- reprinted faithfully and the API refuses rather than guessing; the column is
-- deliberately nullable instead of backfilled, because a backfill would invent
-- exactly the drifted content this is meant to prevent.
ALTER TABLE appointment_orders ADD COLUMN document JSONB;

COMMENT ON COLUMN appointment_orders.document IS
    'Frozen render input for this order. NULL = issued before snapshots existed; not reprintable.';
