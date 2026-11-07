package handler

import (
	"github.com/gofiber/fiber/v2"
	"ta-payment-back/internal/service"
)

type DocumentPolicyHandler struct{ Svc *service.Container }

func (h *DocumentPolicyHandler) Get(c *fiber.Ctx) error {
	out, err := h.Svc.Docs.DocumentPolicy(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(out)
}
func (h *DocumentPolicyHandler) Update(c *fiber.Ctx) error {
	var in service.DocumentPolicy
	if err := Bind(c, &in); err != nil {
		return err
	}
	out, err := h.Svc.Docs.UpdateDocumentPolicy(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.JSON(out)
}
