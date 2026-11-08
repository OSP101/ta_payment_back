package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// SuppliersPreview — GET /ta-review/suppliers?term=<id> — the term's new TAs
// and whether each one can go into the finance office's Suppliers file. No
// PII: names and "on file / not on file" only.
func (h *DocsHandler) SuppliersPreview(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid term")
	}
	rows, err := h.Svc.Docs.SupplierCandidates(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(rows)
}

// SuppliersXLSX — POST /ta-review/suppliers.xlsx?term=<id> — the file itself.
// It carries full citizen IDs and bank accounts, so the caller's own TOTP code
// is re-checked first, in the body (never the query string), exactly as the
// transfer cover does.
func (h *DocsHandler) SuppliersXLSX(c *fiber.Ctx) error {
	var in struct {
		Code string `json:"code"`
	}
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.MFA.VerifyStepUp(c.Context(), UserID(c), in.Code); err != nil {
		return err
	}
	termID, err := uuid.Parse(c.Query("term"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid term")
	}
	body, _, err := h.Svc.Docs.BuildSuppliersWorkbook(c.Context(), UserID(c), termID)
	if err != nil {
		return err
	}
	name := "Suppliers-ทีเอใหม่"
	var year, sem string
	if err := h.Svc.Pool.QueryRow(c.Context(),
		`SELECT academic_year::text, semester::text FROM academic_terms WHERE id = $1`, termID,
	).Scan(&year, &sem); err == nil {
		name += "-" + sem + "-" + year
	}
	c.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Set("Content-Disposition", contentDisposition("attachment", name+".xlsx"))
	return c.Send(body)
}
