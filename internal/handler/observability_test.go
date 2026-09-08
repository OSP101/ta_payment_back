package handler

import (
	"errors"
	"testing"

	"github.com/gofiber/fiber/v2"

	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
)

// The access log's severity is the first filter of any incident, so the status
// it records has to be the one the client was actually told.
//
// Fiber runs ErrorHandler OUTSIDE the middleware chain, after every middleware
// has already returned. A logger that reads c.Response().StatusCode() therefore
// sees the status from before the error was handled — 200 — and every 404 and
// 500 gets filed as a successful request at INFO. errorResponse is the shared
// mapper that makes the log and the response agree.
func TestErrorResponse_MatchesWhatTheClientIsTold(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"fiber error keeps its code", fiber.NewError(fiber.StatusNotFound, "nope"), fiber.StatusNotFound},
		{"not found sentinel", service.ErrNotFound, fiber.StatusNotFound},
		{"forbidden sentinel", service.ErrForbidden, fiber.StatusForbidden},
		{"conflict sentinel", service.ErrConflict, fiber.StatusConflict},
		{"invalid input sentinel", service.ErrInvalidInput, fiber.StatusBadRequest},
		// An ASCII error is an internal fault, not advice for a user.
		{"internal error", errors.New("cannot scan NULL into *time.Time"), fiber.StatusInternalServerError},
		// A Thai error is a business message the user is meant to read.
		{"business message", errors.New("ยอดเกินงบที่ตั้งไว้"), fiber.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, msg := errorResponse(c.err)
			if got != c.want {
				t.Errorf("status = %d, want %d — the access log would file this "+
					"request under the wrong severity", got, c.want)
			}
			if msg == "" {
				t.Error("empty message: the client is told nothing at all")
			}
		})
	}
}

// The internal-fault message must stay generic: it is what a 500 shows a user,
// and the real text goes to the log with the request id instead.
func TestErrorResponse_InternalFaultsDoNotLeakTheirText(t *testing.T) {
	_, msg := errorResponse(errors.New(`pq: relation "users" does not exist`))
	if msg != "ระบบขัดข้อง กรุณาลองใหม่ภายหลัง" {
		t.Errorf("internal fault answered %q — driver internals must not reach the client", msg)
	}
}

// An audit row records the role that EXPLAINS the permission, not whichever
// role happens to be first in the account's list. An account that is both a
// lecturer and staff did not sign off a payout as a lecturer.
func TestActingRole_RecordsTheMostPrivilegedRole(t *testing.T) {
	for _, c := range []struct {
		roles []string
		want  string
	}{
		{[]string{rbac.RoleTA}, rbac.RoleTA},
		{[]string{rbac.RoleLecturer, rbac.RoleStaff}, rbac.RoleStaff},
		{[]string{rbac.RoleTA, rbac.RoleAdmin, rbac.RoleLecturer}, rbac.RoleAdmin},
		// Nothing to record rather than a guess — a failed login has no role.
		{nil, ""},
		// A role outside the vocabulary is kept, not dropped: an unknown role
		// is still what the request acted under.
		{[]string{"executive"}, "executive"},
	} {
		if got := actingRole(c.roles); got != c.want {
			t.Errorf("actingRole(%v) = %q, want %q", c.roles, got, c.want)
		}
	}
}
