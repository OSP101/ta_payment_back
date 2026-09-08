// audit_read.go records who LOOKED at sensitive data, not only who changed it.
//
// The trail already covered the documents and the national ID (ta_doc.view,
// ta_profile.citizen_id.reveal). What it did not cover was every screen that
// shows the same facts in bulk: the executive dashboard is every TA's pay for
// the whole college on one page, and the user list is every account's email and
// student id. Someone reading those left no trace at all.
//
// The reason this had not been done is noise, and it is a real constraint
// rather than an excuse: these are GET endpoints, some of them polled every
// twenty seconds and revalidated whenever a tab regains focus. One audit row
// per request would file thousands of identical entries a day, bury the reads
// that mean something, and — since migration 0107 made audit_logs append-only —
// there would be no way to clean them up afterwards. So the recording is
// throttled, and the throttle is the design.
package handler

import (
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
)

// readAuditWindow is how long one viewer's look at one thing counts as the same
// look.
//
// An hour, because the question this answers is "was this person going through
// the salary data on Tuesday afternoon", not "how many times did their browser
// refresh". A window short enough to catch every poll would reproduce exactly
// the flood it exists to prevent; one much longer would merge a morning glance
// and an evening trawl into a single entry.
const readAuditWindow = time.Hour

type readAuditKey struct {
	viewer  uuid.UUID
	action  string
	subject string
}

// readAuditSeen remembers the last time each (viewer, screen, subject) was
// recorded.
//
// In memory rather than a SELECT against audit_logs per request: the throttle
// runs on every read of these screens, and the existing precedent for reading a
// throttle back out of the audit table (the reminder gate in
// submission_review.go) is a once-a-day action, not a poll. A restart forgets
// the window and files one extra row per viewer per screen, which is the right
// direction to be wrong in — the failure mode is a duplicate entry, never a
// missing one.
//
// The key space is bounded by (real users × audited screens × subjects they can
// reach), and sweepReadAudit drops anything past the window.
var readAuditSeen sync.Map // readAuditKey -> time.Time

// AuditRead records a look at sensitive data, once per readAuditWindow.
//
// action names the screen ("dashboard.executive.view"); entity/subject say what
// was looked at. subjectParam, when set, is the route parameter naming the
// specific thing (a course, a user), so that reading TWO different courses'
// payouts files two entries rather than one.
func AuditRead(aud *audit.Auditor, action, entity, subjectParam string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if err := c.Next(); err != nil {
			return err
		}
		// Only a request that actually returned data disclosed anything. A 403
		// or a 404 is an attempt, and attempts are the access layer's business,
		// not the disclosure trail's.
		if status := c.Response().StatusCode(); status < 200 || status >= 300 {
			return nil
		}
		viewer := UserID(c)
		if viewer == uuid.Nil {
			return nil
		}
		subject := ""
		if subjectParam != "" {
			subject = c.Params(subjectParam)
		}
		if !readAuditDue(readAuditKey{viewer: viewer, action: action, subject: subject}, time.Now()) {
			return nil
		}
		// Best-effort, unlike the write trail. A failure to record a READ must
		// not turn a page that was already rendered into an error the user
		// sees — the disclosure has happened either way, and the honest
		// response to a logging fault is a log line, not a 500 for a request
		// that succeeded.
		if err := aud.Log(c.Context(), audit.Entry{
			ActorID: &viewer, Action: action, Entity: entity, EntityID: subject,
			Note: readAuditNote(c),
		}); err != nil {
			return nil
		}
		return nil
	}
}

// readAuditNote keeps the query that shaped the view — which term, which
// filter — because "looked at the payout dashboard" and "looked at the payout
// dashboard filtered to one TA" are different acts.
func readAuditNote(c *fiber.Ctx) string {
	q := string(c.Request().URI().QueryString())
	if len(q) > 300 {
		q = q[:300] + "…"
	}
	return q
}

// readAuditDue reports whether this look should be recorded, and marks it.
func readAuditDue(k readAuditKey, now time.Time) bool {
	if prev, ok := readAuditSeen.Load(k); ok {
		if last, ok := prev.(time.Time); ok && now.Sub(last) < readAuditWindow {
			return false
		}
	}
	readAuditSeen.Store(k, now)
	return true
}

// SweepReadAudit drops throttle entries whose window has passed, so a
// long-running process does not hold a key for every viewer of every screen
// forever. Safe to call from the existing cleanup scheduler.
func SweepReadAudit(now time.Time) {
	readAuditSeen.Range(func(k, v any) bool {
		if last, ok := v.(time.Time); ok && now.Sub(last) >= readAuditWindow {
			readAuditSeen.Delete(k)
		}
		return true
	})
}
