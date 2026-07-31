// Package timeutil pins every calendar decision in this system to Thailand's
// civil time (Asia/Bangkok, UTC+7, no DST).
//
// Why this exists: month boundaries decide money. A work log may not be
// back-dated into a month that has already passed (see
// service.validateWorkLogEntry), submission periods close on a due date, and
// the payout export locks a month. All of those compare "today" against a
// calendar date. If "today" is derived from the host's local zone, a server
// running in UTC reports the previous day between 00:00 and 07:00 Bangkok
// time — a seven-hour window at every month boundary during which a TA could
// back-date into a closed month, and during which the auto-close sweep fires
// against the wrong date.
//
// The Dockerfile does set TZ=Asia/Bangkok, but that makes correctness a
// property of the deployment rather than of the code: a run under `go run`,
// a CI job, a cron container, or a future k8s manifest that forgets the
// variable would silently compute different money. Pinning it here removes
// that class of failure.
//
// The Postgres side is pinned separately — see db.Connect, which sets the
// session `timezone` runtime parameter so CURRENT_DATE agrees with this
// package. Both halves must match; changing one without the other reintroduces
// the same off-by-seven-hours bug in a harder-to-spot form.
package timeutil

import (
	"time"

	// Embed the IANA tz database in the binary. Without this, LoadLocation
	// depends on system tzdata being present, which a scratch/distroless base
	// image would not have. Costs ~450KB and makes the zone lookup total.
	_ "time/tzdata"
)

// Bangkok is Thailand's civil time zone. Loaded once at init; falls back to a
// fixed UTC+7 offset if the embedded database is somehow unavailable, because
// a wrong-but-close zone beats a nil *Location panic in the money path.
// Thailand has observed UTC+7 with no daylight saving since 1920, so the
// fallback is exact for every date this system will ever handle.
var Bangkok = mustBangkok()

func mustBangkok() *time.Location {
	loc, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		return time.FixedZone("+07", 7*60*60)
	}
	return loc
}

// Now returns the current instant expressed in Bangkok civil time. Use this
// instead of time.Now() anywhere the result feeds a calendar comparison
// (year/month/day), which is every deadline and lock in this system.
func Now() time.Time { return time.Now().In(Bangkok) }

// Today returns midnight of the current Bangkok calendar day. Useful when a
// caller wants to compare against a date parsed from "YYYY-MM-DD" without the
// clock component participating.
func Today() time.Time {
	y, m, d := Now().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, Bangkok)
}

// ParseDate reads a "YYYY-MM-DD" calendar date as a Bangkok-local instant at
// midnight. time.Parse without a location yields UTC, which then disagrees
// with Now() by seven hours when the two are compared as instants.
func ParseDate(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, Bangkok)
}
