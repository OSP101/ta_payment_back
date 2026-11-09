package handler

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
)

type worklogReadItem struct {
	AssignmentID uuid.UUID `json:"assignment_id"`
	Entries []service.WorkLog `json:"entries"`
	Changes []service.WorkLogChange `json:"changes"`
}

// ReadBatch combines review HTTP calls, retaining each service's course/owner
// authorization. Fail the whole response on any forbidden assignment.
func (h *WorkLogHandler) ReadBatch(c *fiber.Ctx) error {
	raw := strings.Split(c.Query("assignment_ids"), ",")
	if len(raw) == 0 || len(raw) > 100 { return fiber.NewError(400, "invalid assignment selection") }
	ids := make([]uuid.UUID,0,len(raw))
	seen := map[uuid.UUID]bool{}
	for _, value := range raw {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil { return fiber.NewError(400, "invalid assignment id") }
		if !seen[id] { ids = append(ids,id); seen[id] = true }
	}
	privileged := rbac.Has(Roles(c),rbac.RoleAdmin,rbac.RoleStaff)
	out := make([]worklogReadItem,0,len(ids))
	for _, id := range ids {
		entries, err := h.Svc.WorkLog.List(c.Context(), UserID(c), id, privileged)
		if err != nil { return err }
		changes, err := h.Svc.WorkLog.ListChanges(c.Context(), UserID(c), id, privileged)
		if err != nil { return err }
		out = append(out,worklogReadItem{id,entries,changes})
	}
	c.Set("Cache-Control","private, no-store")
	return c.JSON(out)
}
