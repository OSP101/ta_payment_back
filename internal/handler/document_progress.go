package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

// DocProgressHandler exposes the per-term physical-document progress board.
type DocProgressHandler struct{ Svc *service.Container }

// Get returns the term's document progress + export readiness (?term_id=).
// Readable by any authenticated user — this is the shared status board.
func (h *DocProgressHandler) Get(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid term_id")
	}
	out, err := h.Svc.DocProgress.GetByTerm(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// SetStage moves the term's document progress (staff/admin only; gated on all
// courses being exported).
func (h *DocProgressHandler) SetStage(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	var body struct {
		Stage int    `json:"stage"`
		Note  string `json:"note"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.DocProgress.SetStage(c.Context(), UserID(c), termID, body.Stage, body.Note); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}
