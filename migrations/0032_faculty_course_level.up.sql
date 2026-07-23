-- Faculty courses gain a teaching level (ปริญญาตรี / บัณฑิตศึกษา) so the
-- appointment order (คำสั่งแต่งตั้งทีเอ) can group courses by the COURSE's level
-- instead of by each TA's appointment level. Existing rows default to
-- 'undergrad'; staff correct graduate courses via the catalog form.
ALTER TABLE faculty_courses
    ADD COLUMN IF NOT EXISTS level TEXT NOT NULL DEFAULT 'undergrad'
        CHECK (level IN ('undergrad', 'graduate'));
