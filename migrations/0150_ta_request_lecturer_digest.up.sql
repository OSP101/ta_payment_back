-- Since 0149 a submission naming five TAs is five requests, and the lecturer
-- got one e-mail per TA as each was decided (05/10/2026: "ดูเยอะมาก กลายเป็นดู
-- น่ารำคาญ"). The lecturer now hears once per submission: when every TA on it
-- is decided, or 24 hours after it was sent, whichever comes first.
--
-- lecturer_notified_at: the lecturer has been told this request's verdict.
-- lecturer_note: clash lines held back for that one notice.
ALTER TABLE ta_requests
  ADD COLUMN lecturer_notified_at TIMESTAMPTZ,
  ADD COLUMN lecturer_note TEXT;

-- Everything decided before this was already told under the old rule.
UPDATE ta_requests
   SET lecturer_notified_at = COALESCE(decided_at, updated_at, NOW())
 WHERE status <> 'submitted';
