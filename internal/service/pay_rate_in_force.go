package service

// payRatesInForce is the ONE definition of "the pay rate in force today", as a
// FROM-clause item aliased pay_rates so it drops in where the table name was.
//
// pay_rates is versioned by effective_from, and every reader used to pick
// "ORDER BY effective_from DESC LIMIT 1" — which made effective_from a sort key
// and not a date: a version saved today with next term's date became the live
// rate for every cap, budget, and payout immediately, while the save dialog
// promised it would "start on" that date. Twenty-five copies of that fragment
// is how the filter could be missing from all of them; one definition is how it
// stays present at the twenty-sixth.
//
// CURRENT_DATE is Bangkok's date: every pool pins timezone=Asia/Bangkok
// (internal/db). created_at breaks a same-day tie in favour of the later save.
const payRatesInForce = `(SELECT * FROM pay_rates
	WHERE effective_from <= CURRENT_DATE
	ORDER BY effective_from DESC, created_at DESC LIMIT 1) pay_rates`
