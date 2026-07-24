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

// ListChecklist returns the per-course signature checklist (?term_id=).
// Readable by any authenticated user.
func (h *DocProgressHandler) ListChecklist(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid term_id")
	}
	out, err := h.Svc.DocProgress.ListChecklist(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// ToggleSignature marks a course-role signature done/undone (staff/admin only).
func (h *DocProgressHandler) ToggleSignature(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	var body struct {
		Role   string `json:"role"`
		Signed bool   `json:"signed"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.DocProgress.ToggleSignature(c.Context(), UserID(c), tcID, body.Role, body.Signed); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// RemindUnsigned emails lecturers who still owe a signature (staff/admin only).
func (h *DocProgressHandler) RemindUnsigned(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	n, err := h.Svc.DocProgress.RemindUnsigned(c.Context(), UserID(c), termID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "notified": n})
}
