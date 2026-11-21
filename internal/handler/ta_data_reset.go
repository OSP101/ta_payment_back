package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"ta-payment-back/internal/service"
)

func (h *UserHandler) ResetTAData(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var in service.ResetTADataInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Workload.ResetTAData(c.Context(), UserID(c), id, in); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}
