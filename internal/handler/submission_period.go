package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

// SubmissionPeriodHandler exposes CRUD + TA-facing endpoints for the monthly
// submission workflow introduced by migration 0019.
type SubmissionPeriodHandler struct {
	Svc *service.Container
}

// List returns every period, optionally filtered by ?term_id=.
func (h *SubmissionPeriodHandler) List(c *fiber.Ctx) error {
	var termID uuid.UUID
	if raw := c.Query("term_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid term_id")
		}
		termID = id
	}
	out, err := h.Svc.SubmissionPeriods.List(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// Upsert creates or updates one period. Body follows the SubmissionPeriod struct.
func (h *SubmissionPeriodHandler) Upsert(c *fiber.Ctx) error {
	var in service.SubmissionPeriod
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	out, err := h.Svc.SubmissionPeriods.Upsert(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// BulkForTerm auto-creates the 5-period template for a given term (มิ.ย.–ต.ค.
// for semester 1, พ.ย.–มี.ค. for semester 2). Idempotent.
func (h *SubmissionPeriodHandler) BulkForTerm(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	out, err := h.Svc.SubmissionPeriods.BulkCreateForTerm(c.Context(), UserID(c), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// Delete removes a period (cascades status rows).
func (h *SubmissionPeriodHandler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.SubmissionPeriods.Delete(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// MePending lists every (period × course) status row this TA still owes,
// used by the TA reminders page.
func (h *SubmissionPeriodHandler) MePending(c *fiber.Ctx) error {
	out, err := h.Svc.SubmissionPeriods.PendingByTA(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// TaSign flips the (period × tcId) status to ta_signed for the calling TA.
func (h *SubmissionPeriodHandler) TaSign(c *fiber.Ctx) error {
	pid, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	if err := h.Svc.SubmissionPeriods.MarkTASigned(c.Context(), UserID(c), pid, tcID); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// LecturerSign flips the row to lecturer_signed for the given TA.
func (h *SubmissionPeriodHandler) LecturerSign(c *fiber.Ctx) error {
	pid, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	taID, err := uuid.Parse(c.Params("taId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid taId")
	}
	if err := h.Svc.SubmissionPeriods.MarkLecturerSigned(c.Context(), UserID(c), pid, taID, tcID); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}
