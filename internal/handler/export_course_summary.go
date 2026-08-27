package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

// CourseSummaryWarnings — GET /exports/terms/:id/course-summary/warnings —
// lets the staff screen show "N รายการที่ควรตรวจสอบ" before the download,
// without generating (and discarding) the workbook itself. The summary is an
// estimate document and is never blocked by these — see
// ExportService.BuildCourseSummaryWorkbook's own doc comment.
func (h *TeachingHandler) CourseSummaryWarnings(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	warnings, err := h.Svc.Export.CourseSummaryWarnings(c.Context(), termID)
	if err != nil {
		return err
	}
	if warnings == nil {
		warnings = []string{}
	}
	return c.JSON(fiber.Map{"warnings": warnings})
}

// CourseSummaryXLSX — GET /exports/terms/:id/course-summary.xlsx — the
// "สรุปรายวิชาที่ขอใช้ TA" budget-request workbook. Records a ledger row (see
// ExportService.BuildCourseSummaryWorkbook) so this generation can be
// reprinted later even if the underlying data changes.
func (h *TeachingHandler) CourseSummaryXLSX(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	body, _, err := h.Svc.Export.BuildCourseSummaryWorkbook(c.Context(), UserID(c), termID)
	if err != nil {
		return err
	}
	name := "course-summary.xlsx"
	var year, sem string
	if err := h.Svc.Pool.QueryRow(c.Context(),
		`SELECT academic_year::text, semester::text FROM academic_terms WHERE id = $1`, termID,
	).Scan(&year, &sem); err == nil {
		name = "course-summary-" + year + "-" + sem + ".xlsx"
	}
	c.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Set("Content-Disposition", contentDisposition("attachment", name))
	return c.Send(body)
}

// CourseSummaryPreview — GET /exports/terms/:id/course-summary/preview — the
// same "สรุปรายวิชาที่ขอใช้ TA" data as a JSON table, for the on-screen summary
// staff asked to see before downloading anything.
func (h *TeachingHandler) CourseSummaryPreview(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	sheets, warnings, err := h.Svc.Export.CourseSummaryPreview(c.Context(), termID)
	if err != nil {
		return err
	}
	if sheets == nil {
		sheets = []service.CourseSummaryPreviewSheet{}
	}
	if warnings == nil {
		warnings = []string{}
	}
	return c.JSON(fiber.Map{"sheets": sheets, "warnings": warnings})
}

// CourseSummaryHistory — GET /exports/terms/:id/course-summary/history — the
// generation ledger: who, when, how many courses, reprintable by id.
func (h *TeachingHandler) CourseSummaryHistory(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.Export.ListCourseSummaryExports(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// CourseSummaryReprint — GET /exports/course-summary/:id/reprint — re-renders
// a past generation from its frozen snapshot rather than recomputing from
// today's tables.
func (h *TeachingHandler) CourseSummaryReprint(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	body, err := h.Svc.Export.ReprintCourseSummary(c.Context(), UserID(c), id)
	if err != nil {
		return err
	}
	c.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Set("Content-Disposition", contentDisposition("attachment", "course-summary.xlsx"))
	return c.Send(body)
}
