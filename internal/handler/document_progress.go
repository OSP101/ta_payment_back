package handler

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

// DocProgressHandler exposes the per-term physical-document progress board.
type DocProgressHandler struct{ Svc *service.Container }

// roundParam reads ?round=, defaulting to 1 — the round every term had before
// the fiscal-round split (12/08/2026) and the only round a non-crossing term
// ever has, so an older client (or a request that never mentions round) keeps
// getting exactly what it always got.
func roundParam(c *fiber.Ctx) (int, error) {
	raw := c.Query("round", "1")
	switch raw {
	case "1":
		return 1, nil
	case "2":
		return 2, nil
	default:
		return 0, fiber.NewError(fiber.StatusBadRequest, "round must be 1 or 2")
	}
}

// Get returns every round of the term's document progress (?term_id=).
// Readable by any authenticated user — this is the shared status board.
func (h *DocProgressHandler) Get(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid term_id")
	}
	out, err := h.Svc.DocProgress.GetOverview(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// SetStage moves ONE ROUND of the term's document progress (staff/admin only;
// gated on every course in that round being exported). ?round= defaults to 1.
func (h *DocProgressHandler) SetStage(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	round, err := roundParam(c)
	if err != nil {
		return err
	}
	var body struct {
		Stage int    `json:"stage" validate:"gte=0,lte=5"`
		Note  string `json:"note" validate:"max=500"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
	if err := h.Svc.DocProgress.SetStage(c.Context(), UserID(c), termID, body.Stage, body.Note, round); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// ListChecklist returns one round's per-course signature checklist
// (?term_id=&round=). Readable by any authenticated user.
func (h *DocProgressHandler) ListChecklist(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid term_id")
	}
	round, err := roundParam(c)
	if err != nil {
		return err
	}
	out, err := h.Svc.DocProgress.ListChecklist(c.Context(), termID, round)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// ToggleSignature marks a course-role signature done/undone for one round
// (staff/admin only). ?round= defaults to 1.
func (h *DocProgressHandler) ToggleSignature(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	round, err := roundParam(c)
	if err != nil {
		return err
	}
	var body struct {
		Role string `json:"role" validate:"required,oneof=ta lecturer certifier"`
		// SignerID is nil for "certifier" and required for the other two roles —
		// that pairing is a cross-field business rule ToggleSignature already
		// enforces, so here it's only checked to be a well-formed id when present.
		SignerID *string `json:"signer_id" validate:"omitempty,uuid4"`
		Signed   bool    `json:"signed"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
	var signerID *uuid.UUID
	if body.SignerID != nil && *body.SignerID != "" {
		id, perr := uuid.Parse(*body.SignerID)
		if perr != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid signer_id")
		}
		signerID = &id
	}
	if err := h.Svc.DocProgress.ToggleSignature(c.Context(), UserID(c), tcID, body.Role, signerID, body.Signed, round); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// RemindUnsigned emails lecturers who still owe a signature for one round
// (staff/admin only). ?round= defaults to 1.
func (h *DocProgressHandler) RemindUnsigned(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	round, err := roundParam(c)
	if err != nil {
		return err
	}
	n, err := h.Svc.DocProgress.RemindUnsigned(c.Context(), UserID(c), termID, round)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "notified": n})
}

/* -------------------------------------------------------------------------- */
/* Public share links                                                        */
/* -------------------------------------------------------------------------- */

// GetShareLink returns the term's current live public link, if staff has
// issued one (staff/admin only — an officer checking what they already
// posted, or whether they need to make one).
func (h *DocProgressHandler) GetShareLink(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	link, err := h.Svc.DocProgress.GetShareLink(c.Context(), termID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return c.JSON(nil)
		}
		return err
	}
	return c.JSON(link)
}

// CreateShareLink issues (or returns the already-live) public link for the
// term (staff/admin only).
func (h *DocProgressHandler) CreateShareLink(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	link, err := h.Svc.DocProgress.CreateShareLink(c.Context(), UserID(c), termID)
	if err != nil {
		return err
	}
	return c.JSON(link)
}

// RevokeShareLink turns off the term's live public link (staff/admin only).
func (h *DocProgressHandler) RevokeShareLink(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	if err := h.Svc.DocProgress.RevokeShareLink(c.Context(), UserID(c), termID); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบลิงก์ที่ใช้งานอยู่")
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

/* -------------------------------------------------------------------------- */
/* Public read (no authentication)                                           */
/* -------------------------------------------------------------------------- */

// PublicGet — GET /public/document-progress/:linkId — the anonymous view a
// staff-issued share link resolves to. Same shape as the authenticated board
// (every round, showFinalStage on), because a public reader is not a TA the
// stepper needs to hide the dean's step from.
func (h *DocProgressHandler) PublicGet(c *fiber.Ctx) error {
	linkID, err := uuid.Parse(c.Params("linkId"))
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "ไม่พบลิงก์")
	}
	termID, termLabel, err := h.Svc.DocProgress.PublicResolveTerm(c.Context(), linkID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบลิงก์ หรือลิงก์นี้ถูกยกเลิกแล้ว")
		}
		return err
	}
	out, err := h.Svc.DocProgress.GetOverview(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"term_id": out.TermID, "term_label": termLabel, "rounds": out.Rounds})
}

// PublicListChecklist — GET /public/document-progress/:linkId/checklist —
// the anonymous counterpart of ListChecklist, scoped to whatever term the
// link points at (?round= defaults to 1, same as the authenticated route).
func (h *DocProgressHandler) PublicListChecklist(c *fiber.Ctx) error {
	linkID, err := uuid.Parse(c.Params("linkId"))
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "ไม่พบลิงก์")
	}
	round, err := roundParam(c)
	if err != nil {
		return err
	}
	termID, _, err := h.Svc.DocProgress.PublicResolveTerm(c.Context(), linkID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบลิงก์ หรือลิงก์นี้ถูกยกเลิกแล้ว")
		}
		return err
	}
	out, err := h.Svc.DocProgress.ListChecklist(c.Context(), termID, round)
	if err != nil {
		return err
	}
	return c.JSON(out)
}
