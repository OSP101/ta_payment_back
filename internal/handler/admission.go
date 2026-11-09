package handler

import "github.com/gofiber/fiber/v2"

// Bound aggregate PDF/ZIP/scan memory across users and demo slots. Per-user
// rate limiting alone does not constrain simultaneous expensive operations.
var heavyOperations = make(chan struct{}, 2)

func heavyAdmission(next fiber.Handler) fiber.Handler {
	return func(c *fiber.Ctx) error {
		select {
		case heavyOperations <- struct{}{}:
			defer func() { <-heavyOperations }()
			return next(c)
		default:
			c.Set("Retry-After", "5")
			return fiber.NewError(fiber.StatusServiceUnavailable, "ระบบกำลังประมวลผลเอกสาร กรุณาลองใหม่สักครู่")
		}
	}
}
