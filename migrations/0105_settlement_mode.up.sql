-- How a course's budget is cut when the work costs more than the money.
--
-- Until now there was one rule, in budget_settlement.go: คาบ are paid in
-- chronological order until the pool runs out, and everything from the first
-- คาบ that does not fit is unpaid. The consequence is that the tail of the term
-- is paid nothing at all — a TA who worked every month is paid for June to
-- September and gets zero for October.
--
-- 'spread' is the alternative a lecturer may turn on when they know the budget
-- can carry it: the pool is divided equally between the months that have work,
-- each month is filled chronologically out of its own share, and whatever no
-- month could spend is handed back out in calendar order so nothing is wasted.
-- Every month with work then receives something, at the cost of no single month
-- being guaranteed in full.
--
-- DEFAULT 'chronological' on purpose: every course that exists today, and every
-- document already issued from one, must settle to exactly the figure it
-- settles to now.
ALTER TABLE teaching_courses
    ADD COLUMN settlement_mode TEXT NOT NULL DEFAULT 'chronological'
        CHECK (settlement_mode IN ('chronological', 'spread')),
    ADD COLUMN settlement_mode_by UUID REFERENCES users(id),
    ADD COLUMN settlement_mode_at TIMESTAMPTZ;

COMMENT ON COLUMN teaching_courses.settlement_mode IS
    'chronological = pay คาบ in date order until the budget runs out (the '
    'original rule). spread = divide the budget equally across the months that '
    'have work so every month is paid something. Changes which คาบ are paid, '
    'never how much any คาบ is worth — the claim form still prints hours × rate.';
COMMENT ON COLUMN teaching_courses.settlement_mode_by IS
    'Who last changed the mode. Money moves between people when it changes, so '
    'the decision carries a name.';
