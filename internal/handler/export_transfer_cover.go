package handler

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

type transferCoverStepUpReq struct {
	Password string `json:"password"`
}

// transferCoverStepUp re-checks the caller's own password before any
// ปะหน้าจ่ายตรง file leaves the server. The file carries every TA's full
// citizen ID (the PromptPay column), so a session left open on an office
// machine must not be enough to walk away with it — the same step-up
// RevealCitizenID asks of a TA viewing only their own number. The password
// travels in a POST body, never the query string, so it stays out of access
// logs and browser history.
func (h *ExportHandler) transferCoverStepUp(c *fiber.Ctx) error {
	var in transferCoverStepUpReq
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	return service.VerifyUserPassword(c.Context(), h.Svc.Pool, UserID(c), in.Password)
}

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

// levelParam reads and validates the ?level= selection every transfer-cover
// endpoint has needed since the level split (12/08/2026): the document is two
// separate files now (ป.ตรี ทำเบิกไม่เหมือนบัณฑิต), so unlike months there is
// no "give me everything" default to fall back to — a caller must say which
// file it wants.
func levelParam(c *fiber.Ctx) (string, error) {
	level := c.Query("level")
	if level != "undergrad" && level != "graduate" {
		return "", fiber.NewError(fiber.StatusBadRequest, "level must be 'undergrad' or 'graduate'")
	}
	return level, nil
}

// levelLabelTH names the file for a Content-Disposition filename.
func levelLabelTH(level string) string {
	if level == "graduate" {
		return "บัณฑิต"
	}
	return "ปตรี"
}

// TransferCoverXLSX — POST /exports/terms/:id/transfer-cover.xlsx — the
// "ปะหน้าจ่ายตรง" (แจ้งโอนจ่ายตรงเข้าบัญชีบุคลากร) document. Refuses outright
// (400, not a 200 with a warnings list) if any course in the term has not
// reached finance_sent — see ExportService.TermExportBlockers.
func (h *ExportHandler) TransferCoverXLSX(c *fiber.Ctx) error {
	if err := h.transferCoverStepUp(c); err != nil {
		return err
	}
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	level, err := levelParam(c)
	if err != nil {
		return err
	}
	months := monthsParam(c)
	body, _, err := h.Svc.Export.BuildTransferCoverWorkbook(c.Context(), UserID(c), termID, months, level)
	if err != nil {
		return err
	}
	name := "transfer-cover-" + levelLabelTH(level)
	var year, sem string
	if err := h.Svc.Pool.QueryRow(c.Context(),
		`SELECT academic_year::text, semester::text FROM academic_terms WHERE id = $1`, termID,
	).Scan(&year, &sem); err == nil {
		name += "-" + year + "-" + sem
	}
	// Name the slice in the filename — two files for one term that differ only
	// by month must not arrive in a downloads folder under the same name.
	if len(months) > 0 {
		name += "-" + months[0]
		if len(months) > 1 {
			name += "_" + months[len(months)-1]
		}
	}
	name += ".xlsx"
	c.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Set("Content-Disposition", contentDisposition("attachment", name))
	return c.Send(body)
}

// TransferCoverBundleZIP — POST /exports/terms/:id/transfer-cover-bundle.zip —
// one click, one zip, both level files inside (whichever are ready). Staff
// asked for a single download button instead of two separate ones crowding
// the header (12/08/2026) — the two documents are still built, gated, and
// ledgered independently exactly as before; only the button is merged.
func (h *ExportHandler) TransferCoverBundleZIP(c *fiber.Ctx) error {
	if err := h.transferCoverStepUp(c); err != nil {
		return err
	}
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	months := monthsParam(c)
	body, _, err := h.Svc.Export.BuildTransferCoverBundle(c.Context(), UserID(c), termID, months)
	if err != nil {
		return err
	}
	name := "transfer-cover"
	var year, sem string
	if err := h.Svc.Pool.QueryRow(c.Context(),
		`SELECT academic_year::text, semester::text FROM academic_terms WHERE id = $1`, termID,
	).Scan(&year, &sem); err == nil {
		name += "-" + year + "-" + sem
	}
	if len(months) > 0 {
		name += "-" + months[0]
		if len(months) > 1 {
			name += "_" + months[len(months)-1]
		}
	}
	name += ".zip"
	c.Set("Content-Type", "application/zip")
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
	level, err := levelParam(c)
	if err != nil {
		return err
	}
	blockers, err := h.Svc.Export.TermExportBlockers(c.Context(), termID, monthsParam(c), level)
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
	level, err := levelParam(c)
	if err != nil {
		return err
	}
	out, err := h.Svc.Export.TransferCoverCoverage(c.Context(), termID, level)
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
	// level is optional here (unlike the other endpoints) — "" lists every
	// generation regardless of level, for a caller that wants the combined
	// history rather than one file's.
	level := c.Query("level")
	if level != "" && level != "undergrad" && level != "graduate" {
		return fiber.NewError(fiber.StatusBadRequest, "level must be 'undergrad' or 'graduate'")
	}
	out, err := h.Svc.Export.ListTransferCoverExports(c.Context(), termID, level)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// TransferCoverReprint — POST /exports/transfer-cover/:id/reprint — re-renders
// a past generation from its frozen snapshot rather than recomputing from
// today's tables.
func (h *ExportHandler) TransferCoverReprint(c *fiber.Ctx) error {
	if err := h.transferCoverStepUp(c); err != nil {
		return err
	}
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
	level, err := levelParam(c)
	if err != nil {
		return err
	}
	sheets, warnings, err := h.Svc.Export.TransferCoverPreview(c.Context(), termID, monthsParam(c), level)
	if err != nil {
		return err
	}
	if warnings == nil {
		warnings = []string{}
	}
	return c.JSON(fiber.Map{"sheets": sheets, "warnings": warnings})
}
