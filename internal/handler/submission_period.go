package handler

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/rbac"
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
	if err := Bind(c, &in); err != nil {
		return err
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
	Comment string `json:"comment" validate:"max=500"`
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
	if err := Bind(c, &body); err != nil {
		return err
	}
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
	if err := Bind(c, &body); err != nil {
		return err
	}
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
		// ToStatus defaults to "pending" when empty (MarkSentBack), so it is
		// not required — but when present it must be a real backward target;
		// "finance_sent" is excluded there, not here, since the service also
		// needs to reject it against the row's CURRENT status.
		ToStatus string `json:"to_status" validate:"omitempty,oneof=pending staff_reviewed exported"`
		Reason   string `json:"reason" validate:"required,max=500"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
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
		Reason string `json:"reason" validate:"required,max=500"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
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

// RemindLecturer — POST /submission-periods/courses/:tcId/remind-lecturer.
// Staff nudge the course's lecturers about worklog rows still awaiting their
// approval. Throttled to once a day per course by the service.
func (h *SubmissionPeriodHandler) RemindLecturer(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	if err := h.Svc.SubmissionPeriods.RemindLecturerUnapproved(c.Context(), UserID(c), tcID); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// worklogEditMaxBytes caps one evidence image. Same 5MB as the announcement
// composer — a phone photo of a printed timetable fits comfortably.
const worklogEditMaxBytes = 5 * 1024 * 1024

// StaffEditBatch — POST /submission-periods/courses/:tcId/tas/:taId/worklog-batch
//
// multipart/form-data:
//
//	payload  JSON {year_month, reason, password, changes:[…]}
//	file     up to 3 images (repeated field)
//
// Multipart rather than JSON because the evidence is the point: a correction
// that rewrites approved hours should carry what the officer was looking at, and
// a two-step "upload then submit" leaves orphan files whenever the second step
// fails.
func (h *SubmissionPeriodHandler) StaffEditBatch(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	taID, err := uuid.Parse(c.Params("taId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid taId")
	}

	var payload struct {
		YearMonth string               `json:"year_month"`
		Reason    string               `json:"reason"`
		Password  string               `json:"password"`
		Changes   []service.EditChange `json:"changes"`
	}
	raw := c.FormValue("payload")
	if raw == "" {
		return fiber.NewError(fiber.StatusBadRequest, "payload required")
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid payload")
	}

	form, err := c.MultipartForm()
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid multipart body")
	}
	files := form.File["file"]
	if len(files) > 3 {
		return fiber.NewError(fiber.StatusBadRequest, "แนบรูปได้ไม่เกิน 3 รูป")
	}

	// Files are stored BEFORE the batch runs so the service can record their keys
	// in the same call. If the batch is then refused, these are orphans in the
	// object store — accepted deliberately: a stored blob nobody references is a
	// cleanup problem, while a recorded batch whose evidence failed to save is an
	// audit trail that lies.
	var evidence []service.EditEvidence
	for _, fh := range files {
		if fh.Size > worklogEditMaxBytes {
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "ไฟล์ใหญ่เกิน 5MB")
		}
		mime := strings.ToLower(fh.Header.Get("Content-Type"))
		ext, ok := announceImageMIME[mime]
		if !ok {
			return fiber.NewError(fiber.StatusUnsupportedMediaType, "รองรับเฉพาะ JPEG / PNG / WebP")
		}
		src, err := fh.Open()
		if err != nil {
			return err
		}
		head := make([]byte, 12)
		n, _ := src.Read(head)
		if !imageMagicMatches(head[:n], ext) {
			src.Close()
			return fiber.NewError(fiber.StatusUnsupportedMediaType, "ประเภทไฟล์ไม่ตรงกับเนื้อหา")
		}
		if _, err := src.Seek(0, 0); err != nil {
			src.Close()
			return err
		}
		key, size, err := h.Svc.Storage.Save("worklog-edits", uuid.New().String()+ext, src)
		src.Close()
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		evidence = append(evidence, service.EditEvidence{
			StorageKey: key, Filename: fh.Filename, Size: size, MIME: mime,
		})
	}

	res, err := h.Svc.WorkLog.ApplyStaffEditBatch(c.Context(), UserID(c), service.EditBatchInput{
		TeachingCourseID: tcID,
		TAID:             taID,
		YearMonth:        payload.YearMonth,
		Reason:           payload.Reason,
		Password:         payload.Password,
		Changes:          payload.Changes,
		Evidence:         evidence,
	})
	if err != nil {
		return err
	}
	return c.JSON(res)
}

// StaffEditHistory — GET …/worklog-batches : the corrections already made to
// this (TA, course), newest first, so the officer can see what a colleague
// changed before changing it again.
func (h *SubmissionPeriodHandler) StaffEditHistory(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid tcId")
	}
	taID, err := uuid.Parse(c.Params("taId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid taId")
	}
	// Route is staffOrLecturer (router.go), and ListEditBatches itself takes
	// no actor — a lecturer may only read their own course's correction
	// history (reason text, before/after hours, evidence file URLs);
	// staff/admin see all.
	if !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		owns, err := h.Svc.Teaching.LecturerOwnsCourse(c.Context(), UserID(c), tcID)
		if err != nil {
			return err
		}
		if !owns {
			return fiber.NewError(fiber.StatusForbidden, "forbidden")
		}
	}
	out, err := h.Svc.WorkLog.ListEditBatches(c.Context(), tcID, taID, c.Query("year_month"))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": out})
}

// ServeEditFile streams one evidence image behind the same auth wall as the rest
// of the API.
//
// The prefix check is the security boundary, not a tidiness rule: without it the
// key parameter is a read primitive over the whole object store, which also
// holds TA national-ID scans.
//
// The key itself is an unguessable random UUID, but "unguessable" is not the
// same as "authorized" — route is staffOrLecturer (router.go), so a lecturer
// still must be checked against the SPECIFIC course this file's batch
// belongs to, the same as StaffEditHistory above. Without this, a lecturer
// who obtained another course's file key (e.g. from a leaked link, or by
// widening StaffEditHistory's own scope) could fetch the underlying image —
// evidence photos can include TA-identifying material.
func (h *SubmissionPeriodHandler) ServeEditFile(c *fiber.Ctx) error {
	key := c.Params("*")
	if key == "" || strings.Contains(key, "..") || !strings.HasPrefix(key, "worklog-edits/") {
		return fiber.NewError(fiber.StatusBadRequest, "invalid key")
	}
	if !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		tcID, err := h.Svc.WorkLog.EditFileCourse(c.Context(), key)
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบรูปภาพ")
		}
		if err != nil {
			return err
		}
		owns, err := h.Svc.Teaching.LecturerOwnsCourse(c.Context(), UserID(c), tcID)
		if err != nil {
			return err
		}
		if !owns {
			return fiber.NewError(fiber.StatusForbidden, "forbidden")
		}
	}
	rc, err := h.Svc.Storage.Open(key)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "ไม่พบรูปภาพ")
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	k := strings.TrimSuffix(key, ".enc")
	// Guard the slice: LastIndex on an extensionless key returns -1 and
	// k[-1:] panics.
	ext := ""
	if dot := strings.LastIndex(k, "."); dot >= 0 {
		ext = strings.ToLower(k[dot:])
	}
	switch ext {
	case ".jpg", ".jpeg":
		c.Set("Content-Type", "image/jpeg")
	case ".png":
		c.Set("Content-Type", "image/png")
	case ".webp":
		c.Set("Content-Type", "image/webp")
	default:
		c.Set("Content-Type", "application/octet-stream")
	}
	c.Set("Cache-Control", "private, max-age=3600")
	c.Set("X-Content-Type-Options", "nosniff")
	return c.Send(body)
}
