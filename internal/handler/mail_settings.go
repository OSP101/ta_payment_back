package handler

import (
	"github.com/gofiber/fiber/v2"

	"ta-payment-back/internal/service"
)

// MailSettingsHandler serves ตั้งค่า > อีเมลแจ้งเตือน: the contact block printed
// at the foot of every notification e-mail.
type MailSettingsHandler struct{ Svc *service.Container }

func (h *MailSettingsHandler) Get(c *fiber.Ctx) error {
	out, err := h.Svc.MailSettings.Get(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(out)
}

type mailContactBody struct {
	Heading string `json:"contact_heading" validate:"max=1000"`
	Unit    string `json:"contact_unit" validate:"max=2000"`
	Detail  string `json:"contact_detail" validate:"max=4000"`
}

func (b mailContactBody) contact() service.MailContact {
	return service.MailContact{Heading: b.Heading, Unit: b.Unit, Detail: b.Detail}
}

func (h *MailSettingsHandler) Update(c *fiber.Ctx) error {
	var in mailContactBody
	if err := Bind(c, &in); err != nil {
		return err
	}
	out, err := h.Svc.MailSettings.Update(c.Context(), UserID(c), in.contact())
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// Preview returns a sample e-mail rendered with the (unsaved) values in the
// body, as JSON {html}, for the settings page to show in a sandboxed iframe.
func (h *MailSettingsHandler) Preview(c *fiber.Ctx) error {
	var in mailContactBody
	if err := Bind(c, &in); err != nil {
		return err
	}
	html, err := h.Svc.MailSettings.Preview(in.contact())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"html": html})
}
