package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

// DataDeletionHandler backs the self-service PDPA erasure-request workflow —
// see internal/service/data_deletion.go for the actual review/approval logic.
type DataDeletionHandler struct{ Svc *service.Container }

type requestDeletionReq struct {
	Reason string `json:"reason" validate:"omitempty,max=1000"`
}

// RequestDeletion is the TA-facing submit endpoint. One pending request per
// user at a time — a second submission while one is live 409s, enforced by
// the DB (see DataDeletionService.RequestDeletion's own comment).
func (h *DataDeletionHandler) RequestDeletion(c *fiber.Ctx) error {
	var in requestDeletionReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	if err := h.Svc.DataDeletion.RequestDeletion(c.Context(), UserID(c), in.Reason); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// MyRequest lets a TA check the live status of their own request (or null if
// they've never submitted one) — /account/my-data polls this to decide
// whether to show the submit form or the outcome.
func (h *DataDeletionHandler) MyRequest(c *fiber.Ctx) error {
	req, err := h.Svc.DataDeletion.MyDeletionRequest(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(req)
}

// ListRequests is the admin review queue. RequireRole(rbac.RoleAdmin) only —
// see router.go's comment on why deletion review is admin-only rather than
// the adminOrStaff bar most review queues use.
func (h *DataDeletionHandler) ListRequests(c *fiber.Ctx) error {
	out, err := h.Svc.DataDeletion.ListDeletionRequests(c.Context(), c.Query("status"))
	if err != nil {
		return err
	}
	return c.JSON(out)
}

type reviewDeletionReq struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note" validate:"omitempty,max=1000"`
}

func (h *DataDeletionHandler) Review(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var in reviewDeletionReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	if err := h.Svc.DataDeletion.ReviewDeletion(c.Context(), UserID(c), id, in.Approve, in.Note); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}
