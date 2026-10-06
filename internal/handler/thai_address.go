package handler

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/gofiber/fiber/v2"

	"ta-payment-back/internal/service"
)

// thaiAddressETag changes only when the embedded list does (a new build).
var thaiAddressETag = func() string {
	sum := sha256.Sum256(service.ThaiAddressJSON())
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}()

// ThaiAddress — GET /thai-address — the province/district/sub-district list
// for the address pickers (service/thai_address.go). Public reference data,
// but served behind login like the rest of the API; cached by the browser.
func (h *DocsHandler) ThaiAddress(c *fiber.Ctx) error {
	c.Set("ETag", thaiAddressETag)
	c.Set("Cache-Control", "private, max-age=86400")
	if c.Get("If-None-Match") == thaiAddressETag {
		return c.SendStatus(fiber.StatusNotModified)
	}
	c.Set("Content-Type", "application/json; charset=utf-8")
	return c.Send(service.ThaiAddressJSON())
}
