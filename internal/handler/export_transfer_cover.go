package handler

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// monthsParam reads the ?months=2026-06,2026-07 selection shared by the
// transfer-cover endpoints. Absent or blank means the whole term, which is
// what every caller meant before the fiscal-year split existed — so an older
// client keeps getting exactly the document it always got.
func monthsParam(c *fiber.Ctx) []string {
	raw := strings.TrimSpace(c.Query("months"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TransferCoverXLSX — GET /exports/terms/:id/transfer-cover.xlsx — the
// "ปะหน้าจ่ายตรง" (แจ้งโอนจ่ายตรงเข้าบัญชีบุคลากร) document. Refuses outright
// (400, not a 200 with a warnings list) if any course in the term has not
// reached finance_sent — see ExportService.TermExportBlockers.
func (h *ExportHandler) TransferCoverXLSX(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	months := monthsParam(c)
	body, _, err := h.Svc.Export.BuildTransferCoverWorkbook(c.Context(), UserID(c), termID, months)
	if err != nil {
		return err
	}
	name := "transfer-cover.xlsx"
	var year, sem string
	if err := h.Svc.Pool.QueryRow(c.Context(),
		`SELECT academic_year::text, semester::text FROM academic_terms WHERE id = $1`, termID,
	).Scan(&year, &sem); err == nil {
		name = "transfer-cover-" + year + "-" + sem + ".xlsx"
	}
	// Name the slice in the filename — two files for one term that differ only
	// by month must not arrive in a downloads folder under the same name.
	if len(months) > 0 {
		name = strings.TrimSuffix(name, ".xlsx") + "-" + months[0]
		if len(months) > 1 {
			name += "_" + months[len(months)-1]
		}
		name += ".xlsx"
	}
	c.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Set("Content-Disposition", contentDisposition("attachment", name))
	return c.Send(body)
}

// TransferCoverBlockers — GET /exports/terms/:id/transfer-cover/blockers —
// lets the staff screen show exactly what is still outstanding before the
// button is even pressed, instead of staff learning about it from a 400.
func (h *ExportHandler) TransferCoverBlockers(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	blockers, err := h.Svc.Export.TermExportBlockers(c.Context(), termID, monthsParam(c))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"blockers": blockers})
}

// TransferCoverCoverage — GET /exports/terms/:id/transfer-cover/coverage —
// the term's claimable months with Thai labels, which have already been
// issued, and where the budget year cuts the term. Drives the month picker and
// the "ยังไม่ได้ออก" warning.
func (h *ExportHandler) TransferCoverCoverage(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.Export.TransferCoverCoverage(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// TransferCoverHistory — GET /exports/terms/:id/transfer-cover/history — the
// generation ledger: who, when, how much, reprintable by id.
func (h *ExportHandler) TransferCoverHistory(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.Export.ListTransferCoverExports(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// TransferCoverReprint — GET /exports/transfer-cover/:id/reprint — re-renders
// a past generation from its frozen snapshot rather than recomputing from
// today's tables.
func (h *ExportHandler) TransferCoverReprint(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	body, err := h.Svc.Export.ReprintTransferCover(c.Context(), UserID(c), id)
	if err != nil {
		return err
	}
	c.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Set("Content-Disposition", contentDisposition("attachment", "transfer-cover.xlsx"))
	return c.Send(body)
}

// TransferCoverPreview — GET /exports/terms/:id/transfer-cover/preview — the
// same ปะหน้าจ่ายตรง data as a JSON table (no PromptPay column — see
// ExportService.TransferCoverPreview), not gated by finance_sent so staff can
// track progress before every course is done.
func (h *ExportHandler) TransferCoverPreview(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	sheets, warnings, err := h.Svc.Export.TransferCoverPreview(c.Context(), termID, monthsParam(c))
	if err != nil {
		return err
	}
	if warnings == nil {
		warnings = []string{}
	}
	return c.JSON(fiber.Map{"sheets": sheets, "warnings": warnings})
}
