// observability.go carries the request-scoped identity that makes the audit
// trail followable: a request id, and the actor/session/address every audit row
// written during the request is stamped with.
//
// Before this file, an audit row said WHO and WHAT and nothing else. Measured on
// the live table: 0 of 407 rows had a role, 0 had an IP, and none could be tied
// to the request that produced them or to the process's own log output. See
// migration 0107 for the full before-picture.
package handler

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/rbac"
)

// HeaderRequestID is echoed on every response. It is the one string a user can
// quote in a support ticket that finds everything: the access log line, the
// error, and every audit row the request wrote.
const HeaderRequestID = "X-Request-Id"

// ctxRequestID is where the id lives for the duration of the request.
const ctxRequestID ctxKey = "request_id"

// RequestID mints an id for every request and returns it to the client.
//
// An inbound X-Request-Id is deliberately NOT honoured. The value ends up in a
// database column that an investigation trusts to group rows, and a client that
// can choose it can make its own actions look like someone else's, or collide
// every request onto one id and make the grouping useless. A gateway that wants
// to correlate its own logs can read the id back out of the response header.
func RequestID() fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := uuid.New()
		c.Locals(string(ctxRequestID), id)
		c.Set(HeaderRequestID, id.String())
		return c.Next()
	}
}

// ReqID returns this request's id, or uuid.Nil outside a request.
func ReqID(c *fiber.Ctx) uuid.UUID {
	v, _ := c.Locals(string(ctxRequestID)).(uuid.UUID)
	return v
}

// AuditContext publishes the request's identity where the audit package can
// find it, so that every audit row written anywhere below this middleware picks
// up the role, address, session and request id on its own.
//
// It runs AFTER Authenticated (so there is an identity to publish) and is
// deliberately separate from it: an unauthenticated request still gets an IP and
// a request id on its audit rows, which is exactly what a failed login needs.
func AuditContext() fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Locals(audit.CtxKeyRequestInfo, audit.RequestInfo{
			RequestID: ReqID(c),
			SessionID: SessionID(c),
			ActorRole: actingRole(Roles(c)),
			IP:        c.IP(),
			UserAgent: c.Get("User-Agent"),
			Method:    c.Method(),
			// c.Path(), NOT c.Route().Path. This middleware runs BEFORE the
			// route is matched, so Route() here is the mount point it is
			// attached to — every row came out saying "/api/v1". Path() is the
			// real request path and, unlike OriginalURL(), carries no query
			// string, so nothing that was passed as a parameter lands in a
			// table this migration also made impossible to delete from.
			Path: c.Path(),
		})
		return c.Next()
	}
}

// actingRole collapses an account's roles to the single one an audit row
// records.
//
// Most accounts hold exactly one role and this is a no-op. For the few that
// hold several, the row records the MOST privileged, because that is the one
// that explains how the action was permitted: an account that is both lecturer
// and staff did not approve a payout as a lecturer.
func actingRole(roles []string) string {
	for _, want := range []string{rbac.RoleAdmin, rbac.RoleStaff, rbac.RoleLecturer, rbac.RoleTA} {
		for _, have := range roles {
			if have == want {
				return want
			}
		}
	}
	if len(roles) > 0 {
		return roles[0]
	}
	return ""
}

// AccessLog writes one structured line per request.
//
// This replaces fiber's logger.New(), which printed a human-shaped line with no
// request id, no actor and no severity — greppable by eye and by nothing else.
// Every field here is one an incident actually filters on, and request_id ties
// the line to the audit rows the same request wrote.
func AccessLog() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		// NOT c.Response().StatusCode(). Fiber runs ErrorHandler outside the
		// middleware chain — after every middleware has returned — so on a
		// failed request the response still carries its pre-error status here,
		// which is 200. Asking the same mapper ErrorHandler uses is the only
		// way this line can report what the client was actually told.
		status := c.Response().StatusCode()
		if err != nil {
			status, _ = errorResponse(err)
		}

		// Level by outcome, so `level=ERROR` alone is a useful filter: 5xx is
		// ours to fix, 4xx is the client's, everything else is noise until
		// someone goes looking.
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}

		attrs := []any{
			"request_id", ReqID(c).String(),
			"method", c.Method(),
			"route", c.Route().Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", c.IP(),
			"bytes", len(c.Response().Body()),
		}
		if uid := UserID(c); uid != uuid.Nil {
			attrs = append(attrs, "actor_id", uid.String(), "actor_role", actingRole(Roles(c)))
		}
		if err != nil {
			attrs = append(attrs, "error", err.Error())
		}
		slog.Log(c.Context(), level, "http_request", attrs...)
		return err
	}
}
