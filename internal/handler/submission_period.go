package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

// SubmissionPeriodHandler exposes CRUD + TA-facing endpoints for the monthly
// submission workflow introduced by migration 0019.
type SubmissionPeriodHandler struct {
	Svc *service.Container
}

// List returns every period, optionally filtered by ?term_id=.
func (h *SubmissionPeriodHandler) List(c *fiber.Ctx) error {
	var termID uuid.UUID
	if raw := c.Query("term_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid term_id")
		}
		termID = id
	}
	out, err := h.Svc.SubmissionPeriods.List(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// Upsert creates or updates one period. Body follows the SubmissionPeriod struct.
func (h *SubmissionPeriodHandler) Upsert(c *fiber.Ctx) error {
	var in service.SubmissionPeriod
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	out, err := h.Svc.SubmissionPeriods.Upsert(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// BulkForTerm auto-creates the 5-period template for a given term (มิ.ย.–ต.ค.
// for semester 1, พ.ย.–มี.ค. for semester 2). Idempotent.
func (h *SubmissionPeriodHandler) BulkForTerm(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid termId")
	}
	out, err := h.Svc.SubmissionPeriods.BulkCreateForTerm(c.Context(), UserID(c), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// Delete removes a period (cascades status rows).
func (h *SubmissionPeriodHandler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.SubmissionPeriods.Delete(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// MePending lists every (period × course) status row this TA still owes,
// used by the TA reminders page.
func (h *SubmissionPeriodHandler) MePending(c *fiber.Ctx) error {
	out, err := h.Svc.SubmissionPeriods.PendingByTA(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// commentBody is the shared payload for the finance-send endpoint so staff can
// attach a note that renders next to the signer in the UI.
type commentBody struct {
	Comment string `json:"comment"`
}

// FinanceSend is the last step — staff records the exported batch has been
// handed to the finance office. Requires the row already in 'exported' state.
func (h *SubmissionPeriodHandler) FinanceSend(c *fiber.Ctx) error {
	pid, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	taID, err := uuid.Parse(c.Params("taId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid taId")
	}
	var body commentBody
	_ = c.BodyParser(&body)
	if err := h.Svc.SubmissionPeriods.MarkFinanceSent(c.Context(), UserID(c), pid, taID, tcID, body.Comment); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// ReviewQueue lists the months waiting on staff review for a term — step 3 of
// the staff workflow ("ตรวจสอบเบิกจ่ายค่าตอบแทน").
func (h *SubmissionPeriodHandler) ReviewQueue(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "term_id is required")
	}
	out, err := h.Svc.SubmissionPeriods.ListReviewQueue(c.Context(), termID)
	if err != nil {
		return err
	}
	// Reported alongside the queue so an empty screen can say WHY. The queue only
	// carries TAs whose appointment order has been printed; without this number a
	// term with work waiting looks identical to a term with none.
	awaiting, err := h.Svc.SubmissionPeriods.CountAwaitingAppointment(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": out, "awaiting_appointment": awaiting})
}

// StaffReview records staff sign-off on one TA's month, releasing it for
// export. The export step refuses anything that has not passed through here.
func (h *SubmissionPeriodHandler) StaffReview(c *fiber.Ctx) error {
	pid, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	taID, err := uuid.Parse(c.Params("taId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid taId")
	}
	var body commentBody
	_ = c.BodyParser(&body)
	if err := h.Svc.SubmissionPeriods.MarkStaffReviewed(c.Context(), UserID(c), pid, taID, tcID, body.Comment); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// SendBack bounces the (period, TA, course) status row back to an earlier
// step with a mandatory reason. Staff/admin from any pre-finance state;
// the course's lecturer only from ta_signed/lecturer_signed.
func (h *SubmissionPeriodHandler) SendBack(c *fiber.Ctx) error {
	pid, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	taID, err := uuid.Parse(c.Params("taId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid taId")
	}
	var body struct {
		ToStatus string `json:"to_status"`
		Reason   string `json:"reason"`
	}
	_ = c.BodyParser(&body)
	// Sending back after the period closed is allowed on purpose — that's when
	// corrections surface. The TA can no longer edit a closed month themself,
	// so the actual fix flows through the staff worklog editor.
	if err := h.Svc.SubmissionPeriods.MarkSentBack(c.Context(), UserID(c), pid, taID, tcID, body.ToStatus, body.Reason); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// FinanceRevert is the admin-only unlock for a finance_sent row (status
// reverts to submitted). Requires a reason.
func (h *SubmissionPeriodHandler) FinanceRevert(c *fiber.Ctx) error {
	pid, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	taID, err := uuid.Parse(c.Params("taId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid taId")
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = c.BodyParser(&body)
	if err := h.Svc.SubmissionPeriods.RevertFinanceSent(c.Context(), UserID(c), pid, taID, tcID, body.Reason); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// MonthDetail returns the work-log rows behind one queue cell, plus the
// section timetable to read them against. This is what "ผ่าน" actually approves —
// before it existed the screen showed only a total.
func (h *SubmissionPeriodHandler) MonthDetail(c *fiber.Ctx) error {
	pid, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	taID, err := uuid.Parse(c.Params("taId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid taId")
	}
	out, err := h.Svc.SubmissionPeriods.MonthDetailForReview(c.Context(), pid, tcID, taID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// Timeline returns the approval history for one (period, TA, course). The
// service restricts reads to the TA themself, the course's lecturers, and
// staff/admin.
func (h *SubmissionPeriodHandler) Timeline(c *fiber.Ctx) error {
	pid, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	taID, err := uuid.Parse(c.Params("taId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid taId")
	}
	out, err := h.Svc.SubmissionPeriods.GetTimeline(c.Context(), UserID(c), pid, taID, tcID)
	if err != nil {
		return err
	}
	if out == nil {
		return fiber.NewError(fiber.StatusNotFound, "not found")
	}
	return c.JSON(out)
}

// ListByCourse lists every (TA × period) timeline row for a course, used by
// the lecturer report page and the staff exports dashboard. Optional
// ?period_id= filters to one month.
func (h *SubmissionPeriodHandler) ListByCourse(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	var pid uuid.UUID
	if raw := c.Query("period_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid period_id")
		}
		pid = id
	}
	out, err := h.Svc.SubmissionPeriods.ListByCourse(c.Context(), UserID(c), tcID, pid)
	if err != nil {
		return err
	}
	return c.JSON(out)
}
