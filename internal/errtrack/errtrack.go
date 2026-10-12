// Package errtrack sends the API's 5xx errors and recovered panics to the
// self-hosted GlitchTip (deploy/docker-compose.yml), which speaks the Sentry
// protocol, so the stock sentry-go client does the sending.
//
// Why it exists: a user who hits "ระบบขัดข้อง" can rarely say what they were
// doing. The access log already has the request id, but finding the line means
// someone with shell access grepping container logs. GlitchTip groups the same
// failure into one issue, counts how often it happens and to how many users,
// and e-mails when a new one appears, which is what a test rollout needs.
//
// What is sent is chosen here field by field rather than left to the SDK's
// defaults. The API carries citizen IDs, passport numbers, bank accounts and
// uploaded ID-card scans, so an event carries the route pattern, the method,
// the request id, the actor's user id and role, and the error text. Never a
// request body, a header, a cookie, a query string or the user's name/e-mail.
// The request id is the bridge back to everything else: the access log line
// and every audit row the request wrote carry the same value.
//
// With SENTRY_DSN unset (every developer machine, and any deployment that has
// not created the GlitchTip project yet) Init does nothing and every Capture
// call is a no-op, so nothing here can make the API depend on GlitchTip being
// up. Sending is asynchronous with a bounded queue; a GlitchTip that is down
// costs dropped events, never a slow request.
package errtrack

import (
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/getsentry/sentry-go"
)

// Options configures Init. Empty DSN disables reporting.
type Options struct {
	DSN         string
	Environment string
	// Release names the build, normally the backend repo's short commit, so
	// an issue says which deploy introduced it and whether a fix has shipped.
	Release string
	// Transport replaces the HTTP sender. Tests only.
	Transport sentry.Transport
}

var enabled bool

// Init starts the client. Call once from main, before the server listens.
func Init(o Options) error {
	if o.DSN == "" {
		slog.Info("errtrack: disabled (SENTRY_DSN empty)")
		return nil
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:         o.DSN,
		Environment: o.Environment,
		Release:     o.Release,
		// Off: with it on, the SDK attaches the client IP and other request
		// detail on its own. Everything an event carries is set explicitly in
		// Capture below.
		SendDefaultPII: false,
		// The host name inside the container is a random hex id that changes
		// on every redeploy; as a tag it only splits issues for no reason.
		ServerName:       "ta-payment-api",
		AttachStacktrace: true,
		// Last line of defence: anything that reaches the wire passes here.
		BeforeSend: scrub,
		Transport:  o.Transport,
	})
	if err != nil {
		return fmt.Errorf("errtrack: %w", err)
	}
	enabled = true
	slog.Info("errtrack: enabled", "environment", o.Environment, "release", o.Release)
	return nil
}

// Flush waits up to timeout for queued events to go out. Call on shutdown so
// the error that preceded a crash is not lost with the process.
func Flush(timeout time.Duration) {
	if enabled {
		sentry.Flush(timeout)
	}
}

// Request is what one event says about the HTTP request it came from. The
// handler package fills it in; this package never sees a fiber.Ctx, so it
// cannot accidentally copy more than these fields.
type Request struct {
	ID     string // X-Request-Id, also on the access log and audit rows
	Method string
	// Route is the matched pattern (/api/v1/claims/:id), never the concrete
	// path: a path can carry a share-link token (/p/progress/<token>), and
	// the pattern is what groups the same bug across different records.
	Route    string
	Status   int
	UserID   string // empty when unauthenticated
	UserRole string // most privileged role, as on audit rows
}

// CaptureError reports an error a handler returned. Returns the event id
// ("" when disabled) so the caller can log it next to the request id.
func CaptureError(err error, r Request) string {
	if !enabled || err == nil {
		return ""
	}
	hub := sentry.CurrentHub().Clone()
	hub.ConfigureScope(func(s *sentry.Scope) { applyRequest(s, r) })
	hub.Scope().SetFingerprint([]string{r.Method, r.Route, normalize(err.Error())})
	if id := hub.CaptureException(err); id != nil {
		return string(*id)
	}
	return ""
}

// CapturePanic reports a recovered panic. Must be called from the deferred
// function that recovered it (fiber's recover middleware StackTraceHandler),
// so the stack trace taken here still runs through the line that panicked.
func CapturePanic(recovered any, r Request) string {
	if !enabled {
		return ""
	}
	hub := sentry.CurrentHub().Clone()
	hub.ConfigureScope(func(s *sentry.Scope) {
		applyRequest(s, r)
		s.SetTag("panic", "true")
		s.SetLevel(sentry.LevelFatal)
	})
	if id := hub.Recover(recovered); id != nil {
		return string(*id)
	}
	return ""
}

func applyRequest(s *sentry.Scope, r Request) {
	s.SetTag("request_id", r.ID)
	s.SetTag("route", r.Route)
	s.SetTag("method", r.Method)
	if r.Status != 0 {
		s.SetTag("status", fmt.Sprint(r.Status))
	}
	if r.UserRole != "" {
		s.SetTag("role", r.UserRole)
	}
	if r.UserID != "" {
		// Id only. GlitchTip counts "users affected" by it, and staff can
		// look the account up in the app if they need to call the person.
		s.SetUser(sentry.User{ID: r.UserID})
	}
	// Shown as the issue's subtitle in the list, so the route is readable
	// without opening the issue.
	tx := r.Method + " " + r.Route
	s.AddEventProcessor(func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
		e.Transaction = tx
		return e
	})
}

// Grouping: the SDK's default fingerprint is the stack trace, and every
// handler error is captured from the same spot in ErrorHandler, so all of
// them would fold into one issue. Grouping by route + error text instead
// keeps "claims export fails" and "login fails" apart, while the ids and
// numbers stripped below keep one bug on many records from becoming a
// separate issue per record.
var (
	reUUID   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reNumber = regexp.MustCompile(`\d+`)
	reDigits = regexp.MustCompile(`\d[\d -]{8,}\d`)
)

// maskDigits hides runs of 10+ digits (citizen ID 13, bank account 10-12,
// phone 10): error text is written by code, and code sometimes includes the
// value it failed on. A run that is part of a longer token, like the
// 12-digit tail of a UUID request id, is kept. Same rule as maskDigits in
// ta_payment_front/app/lib/errorReporting.ts.
func maskDigits(s string) string {
	locs := reDigits.FindAllStringIndex(s, -1)
	if locs == nil {
		return s
	}
	isWord := func(b byte) bool {
		return b == '-' || b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	var out []byte
	last := 0
	for _, l := range locs {
		start, end := l[0], l[1]
		if (start > 0 && isWord(s[start-1])) || (end < len(s) && isWord(s[end])) {
			continue
		}
		n := 0
		for i := start; i < end; i++ {
			if s[i] >= '0' && s[i] <= '9' {
				n++
			}
		}
		if n < 10 {
			continue
		}
		out = append(out, s[last:start]...)
		out = append(out, "[digits hidden]"...)
		last = end
	}
	if out == nil {
		return s
	}
	return string(append(out, s[last:]...))
}

func normalize(msg string) string {
	msg = reUUID.ReplaceAllString(msg, "<id>")
	msg = reNumber.ReplaceAllString(msg, "<n>")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// scrub drops anything the SDK may have filled in on its own. Capture never
// sets these, so on a correct build this removes nothing; it is here so a
// future change (an integration, an SDK upgrade, SendDefaultPII flipped by
// mistake) cannot start shipping request contents.
func scrub(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	e.Request = nil
	e.Message = maskDigits(e.Message)
	for i := range e.Exception {
		e.Exception[i].Value = maskDigits(e.Exception[i].Value)
	}
	e.User.Email = ""
	e.User.Username = ""
	e.User.Name = ""
	e.User.IPAddress = ""
	e.ServerName = "ta-payment-api"
	return e
}
