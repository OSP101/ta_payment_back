package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

// DocProgressHandler exposes the per-term physical-document progress board.
type DocProgressHandler struct{ Svc *service.Container }

// roundParam reads ?round=, defaulting to 1 — the round every term had before
// the fiscal-round split (12/08/2026) and the only round a non-crossing term
// ever has, so an older client (or a request that never mentions round) keeps
// getting exactly what it always got.
func roundParam(c *fiber.Ctx) (int, error) {
	raw := c.Query("round", "1")
	switch raw {
	case "1":
		return 1, nil
	case "2":
		return 2, nil
	default:
		return 0, fiber.NewError(fiber.StatusBadRequest, "round must be 1 or 2")
	}
}

// Get returns every round of the term's document progress (?term_id=).
// Readable by any authenticated user — this is the shared status board.
func (h *DocProgressHandler) Get(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid term_id")
	}
	out, err := h.Svc.DocProgress.GetOverview(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// SetStage moves ONE ROUND of the term's document progress (staff/admin only;
// gated on every course in that round being exported). ?round= defaults to 1.
func (h *DocProgressHandler) SetStage(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	round, err := roundParam(c)
	if err != nil {
		return err
	}
	var body struct {
		Stage int    `json:"stage"`
		Note  string `json:"note"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.DocProgress.SetStage(c.Context(), UserID(c), termID, body.Stage, body.Note, round); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// ListChecklist returns one round's per-course signature checklist
// (?term_id=&round=). Readable by any authenticated user.
func (h *DocProgressHandler) ListChecklist(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid term_id")
	}
	round, err := roundParam(c)
	if err != nil {
		return err
	}
	out, err := h.Svc.DocProgress.ListChecklist(c.Context(), termID, round)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// ToggleSignature marks a course-role signature done/undone for one round
// (staff/admin only). ?round= defaults to 1.
func (h *DocProgressHandler) ToggleSignature(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	round, err := roundParam(c)
	if err != nil {
		return err
	}
	var body struct {
		Role     string  `json:"role"`
		SignerID *string `json:"signer_id"`
		Signed   bool    `json:"signed"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	var signerID *uuid.UUID
	if body.SignerID != nil && *body.SignerID != "" {
		id, perr := uuid.Parse(*body.SignerID)
		if perr != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid signer_id")
		}
		signerID = &id
	}
	if err := h.Svc.DocProgress.ToggleSignature(c.Context(), UserID(c), tcID, body.Role, signerID, body.Signed, round); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// RemindUnsigned emails lecturers who still owe a signature for one round
// (staff/admin only). ?round= defaults to 1.
func (h *DocProgressHandler) RemindUnsigned(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	round, err := roundParam(c)
	if err != nil {
		return err
	}
	n, err := h.Svc.DocProgress.RemindUnsigned(c.Context(), UserID(c), termID, round)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "notified": n})
}
