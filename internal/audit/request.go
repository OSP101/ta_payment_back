package audit

import (
	"context"

	"github.com/google/uuid"
)

// RequestInfo is everything the HTTP layer knows about the request an audit row
// belongs to: who was holding the keyboard, from where, on which session, and
// under which of their roles.
//
// It is carried on the request context rather than passed to Log(), because
// there are 122 audit call sites in this codebase and threading seven more
// arguments through every service signature would have guaranteed that most of
// them kept passing nothing — which is exactly the state this replaces. Before
// this existed, 0 of 407 live rows had a role and 0 had an IP.
//
// A caller with no request behind it (the scheduler, a migration, a test)
// simply has no RequestInfo on its context and the fields stay NULL. That is
// the honest record: nobody's browser did it.
type RequestInfo struct {
	// RequestID joins this row to the X-Request-Id the client was handed and to
	// the structured log line the same request emitted.
	RequestID uuid.UUID
	SessionID uuid.UUID
	// ActorRole is the role the request was ACTING under, which for a
	// multi-role account is not the same as "their roles" — see
	// handler.actingRole.
	ActorRole string
	IP        string
	UserAgent string
	Method    string
	Path      string
}

// ctxKeyRequestInfo is a plain string rather than a private struct type on
// purpose: fiber stores request-scoped values through fasthttp's UserValue map,
// which is what *fasthttp.RequestCtx.Value() reads. Handlers pass c.Context()
// straight into the services, so a value set with c.Locals(ctxKeyRequestInfo,…)
// is readable here with no adapter and no change at any call site.
//
// Exported so the handler package can set it; there is no other legitimate
// writer.
const CtxKeyRequestInfo = "audit_request_info"

// WithRequest attaches request information to a plain context. Used by
// non-fiber entry points (jobs that act on a user's behalf) and by tests;
// fiber handlers use c.Locals(CtxKeyRequestInfo, info) instead, which reaches
// the same place.
func WithRequest(ctx context.Context, info RequestInfo) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

type ctxKey struct{}

// RequestFrom pulls request information off a context, from either of the two
// places it can live. Returns the zero value when there is none.
func RequestFrom(ctx context.Context) RequestInfo {
	if ctx == nil {
		return RequestInfo{}
	}
	if v, ok := ctx.Value(ctxKey{}).(RequestInfo); ok {
		return v
	}
	if v, ok := ctx.Value(CtxKeyRequestInfo).(RequestInfo); ok {
		return v
	}
	return RequestInfo{}
}

// fillFromContext copies request information onto an entry, WITHOUT overriding
// anything the caller set explicitly. A handler that already passes
// IP: c.IP() keeps its value; everything it left blank gets filled in.
func (e *Entry) fillFromContext(ctx context.Context) {
	r := RequestFrom(ctx)
	if e.ActorRole == "" {
		e.ActorRole = r.ActorRole
	}
	if e.IP == "" {
		e.IP = r.IP
	}
	if e.UserAgent == "" {
		e.UserAgent = r.UserAgent
	}
	if e.RequestID == uuid.Nil {
		e.RequestID = r.RequestID
	}
	if e.SessionID == uuid.Nil {
		e.SessionID = r.SessionID
	}
	if e.Method == "" {
		e.Method = r.Method
	}
	if e.Path == "" {
		e.Path = r.Path
	}
}
