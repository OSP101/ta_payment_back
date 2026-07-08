package handler

import (
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
)

// -------------------- User --------------------

type UserHandler struct{ Svc *service.Container }

func (h *UserHandler) List(c *fiber.Ctx) error {
	role := c.Query("role")
	search := c.Query("q")
	limit, _ := strconv.Atoi(c.Query("limit", "50"))
	offset, _ := strconv.Atoi(c.Query("offset", "0"))
	// Lecturers may only list TA accounts (used by the "request TA" flow)
	if rbac.Has(Roles(c), rbac.RoleLecturer) && !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		if role != rbac.RoleTA {
			return fiber.NewError(fiber.StatusForbidden, "lecturers may only list TA accounts")
		}
	}
	users, total, err := h.Svc.Users.List(c.Context(), role, search, limit, offset)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": users, "total": total})
}

func (h *UserHandler) Create(c *fiber.Ctx) error {
	var in service.CreateUserInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	// Lecturers may only create TA accounts
	if rbac.Has(Roles(c), rbac.RoleLecturer) && !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		if len(in.Roles) != 1 || in.Roles[0] != rbac.RoleTA {
			return fiber.NewError(fiber.StatusForbidden, "lecturers may only create TA accounts")
		}
	}
	out, err := h.Svc.Users.Create(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(out)
}

func (h *UserHandler) Get(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	u, err := h.Svc.Users.Get(c.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "user not found")
		}
		return err
	}
	return c.JSON(u)
}

func (h *UserHandler) Update(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var in service.UpdateUserInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	u, err := h.Svc.Users.Update(c.Context(), UserID(c), id, in)
	if err != nil {
		return err
	}
	return c.JSON(u)
}

func (h *UserHandler) ResetPassword(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	pw, err := h.Svc.Users.ResetPassword(c.Context(), UserID(c), id)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"temp_password": pw})
}

func (h *UserHandler) Deactivate(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	// Require the caller to re-type the target user's email to confirm.
	var body struct {
		ConfirmEmail string `json:"confirm_email"`
	}
	_ = c.BodyParser(&body)
	if strings.TrimSpace(body.ConfirmEmail) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "confirm_email required")
	}
	email, err := h.Svc.Users.GetEmail(c.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "user not found")
		}
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(body.ConfirmEmail), email) {
		return fiber.NewError(fiber.StatusBadRequest, "confirmation email does not match")
	}
	if err := h.Svc.Users.Deactivate(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// -------------------- Course --------------------

type CourseHandler struct{ Svc *service.Container }

func (h *CourseHandler) List(c *fiber.Ctx) error {
	out, err := h.Svc.Courses.List(c.Context(), c.Query("q"))
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *CourseHandler) Upsert(c *fiber.Ctx) error {
	var in service.FacultyCourse
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	out, err := h.Svc.Courses.Upsert(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *CourseHandler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.Courses.Delete(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *CourseHandler) PayRate(c *fiber.Ctx) error {
	pr, err := h.Svc.Courses.LatestPayRate(c.Context())
	if err != nil {
		return c.JSON(nil)
	}
	return c.JSON(pr)
}

func (h *CourseHandler) CreatePayRate(c *fiber.Ctx) error {
	var in service.PayRate
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	out, err := h.Svc.Courses.UpsertPayRate(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *CourseHandler) HourCaps(c *fiber.Ctx) error {
	out, err := h.Svc.Courses.ListHourCaps(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *CourseHandler) DeleteHourCap(c *fiber.Ctx) error {
	credits, err := strconv.Atoi(c.Params("credits"))
	if err != nil || credits <= 0 {
		return fiber.NewError(fiber.StatusBadRequest, "invalid credits")
	}
	if err := h.Svc.Courses.DeleteHourCap(c.Context(), UserID(c), credits); err != nil {
		if err == service.ErrNotFound {
			return fiber.NewError(fiber.StatusNotFound, "hour cap not found")
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *CourseHandler) UpsertHourCap(c *fiber.Ctx) error {
	var in service.HourCap
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	out, err := h.Svc.Courses.UpsertHourCap(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *CourseHandler) BudgetCap(c *fiber.Ctx) error {
	b, err := h.Svc.Courses.LatestBudgetCap(c.Context())
	if err != nil {
		return c.JSON(nil)
	}
	return c.JSON(b)
}

func (h *CourseHandler) UpsertBudgetCap(c *fiber.Ctx) error {
	var in service.BudgetCap
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	out, err := h.Svc.Courses.UpsertBudgetCap(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// -------------------- Teaching --------------------

type TeachingHandler struct{ Svc *service.Container }

func (h *TeachingHandler) ListTerms(c *fiber.Ctx) error {
	var f service.TermFilter
	if v := c.Query("year"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Year = &n
		}
	}
	if v := c.Query("year_from"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.YearFrom = &n
		}
	}
	if v := c.Query("year_to"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.YearTo = &n
		}
	}
	out, err := h.Svc.Teaching.ListTerms(c.Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *TeachingHandler) TermYearsCount(c *fiber.Ctx) error {
	n, err := h.Svc.Teaching.TermYearsCount(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"count": n})
}

func (h *TeachingHandler) UpsertTerm(c *fiber.Ctx) error {
	var in service.Term
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	out, err := h.Svc.Teaching.UpsertTerm(c.Context(), UserID(c), in)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidInput):
			return fiber.NewError(fiber.StatusBadRequest, "ข้อมูลไม่ถูกต้อง")
		case errors.Is(err, service.ErrNotFound):
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบภาคเรียน")
		case errors.Is(err, service.ErrConflict):
			// Two possible causes: duplicate (year,semester) on create, or year/semester
			// change on update. Both are the client's problem — same status.
			return fiber.NewError(fiber.StatusConflict, "ปีการศึกษา/ภาคซ้ำ หรือแก้ไขคีย์หลักไม่ได้")
		}
		return err
	}
	return c.JSON(out)
}

func (h *TeachingHandler) TermUsage(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	u, err := h.Svc.Teaching.TermUsage(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(u)
}

func (h *TeachingHandler) DeleteTerm(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	usage, err := h.Svc.Teaching.DeleteTerm(c.Context(), UserID(c), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบภาคเรียน")
		}
		if errors.Is(err, service.ErrConflict) {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error": "term_in_use",
				"usage": usage,
			})
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// ListMyTACourses returns teaching courses where the current user (must be a TA)
// is assigned via an approved TA request. Filterable by ?term_id=.
func (h *TeachingHandler) ListMyTACourses(c *fiber.Ctx) error {
	taID := UserID(c)
	var termID *uuid.UUID
	if v := c.Query("term_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			termID = &id
		}
	}
	out, err := h.Svc.Teaching.ListForTA(c.Context(), taID, termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *TeachingHandler) List(c *fiber.Ctx) error {
	var termID *uuid.UUID
	if v := c.Query("term_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			termID = &id
		}
	}
	var lecturerID *uuid.UUID
	if rbac.Has(Roles(c), rbac.RoleLecturer) && !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		lid := UserID(c)
		lecturerID = &lid
	} else if v := c.Query("lecturer_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			lecturerID = &id
		}
	}
	out, err := h.Svc.Teaching.List(c.Context(), termID, lecturerID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *TeachingHandler) Create(c *fiber.Ctx) error {
	var in service.CreateTeachingCourseInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	id, err := h.Svc.Teaching.Create(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": id})
}

func (h *TeachingHandler) Get(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.Teaching.Get(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *TeachingHandler) SetNumStudents(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	// Accept either legacy `num_students` (aggregate) or per-track split.
	// -1 means "no change" so callers can update one track without touching the other.
	body := struct {
		NumStudents        int `json:"num_students"`
		NumStudentsRegular int `json:"num_students_regular"`
		NumStudentsSpecial int `json:"num_students_special"`
	}{NumStudents: -1, NumStudentsRegular: -1, NumStudentsSpecial: -1}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Teaching.SetNumStudents(c.Context(), UserID(c), id,
		body.NumStudents, body.NumStudentsRegular, body.NumStudentsSpecial); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *TeachingHandler) UpdateSettings(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var in service.UpdateSettingsInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Teaching.UpdateSettings(c.Context(), UserID(c), id, in); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// Section CRUD — all three gate on the course NOT being exported. Locked
// courses return 409 so the frontend can render a lock banner.
func (h *TeachingHandler) AddSection(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var in service.AddSectionInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	id, err := h.Svc.Teaching.AddSection(c.Context(), UserID(c), tcID, in)
	if err != nil {
		if errors.Is(err, service.ErrCourseLocked) {
			return fiber.NewError(fiber.StatusConflict, "รายวิชานี้ถูกล็อกหลังส่งออกไฟล์แล้ว")
		}
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": id})
}

func (h *TeachingHandler) UpdateSection(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	sectionID, err := uuid.Parse(c.Params("sectionId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid section id")
	}
	var in service.UpdateSectionInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Teaching.UpdateSection(c.Context(), UserID(c), tcID, sectionID, in); err != nil {
		if errors.Is(err, service.ErrCourseLocked) {
			return fiber.NewError(fiber.StatusConflict, "รายวิชานี้ถูกล็อกหลังส่งออกไฟล์แล้ว")
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *TeachingHandler) DeleteSection(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	sectionID, err := uuid.Parse(c.Params("sectionId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid section id")
	}
	if err := h.Svc.Teaching.DeleteSection(c.Context(), UserID(c), tcID, sectionID); err != nil {
		if errors.Is(err, service.ErrCourseLocked) {
			return fiber.NewError(fiber.StatusConflict, "รายวิชานี้ถูกล็อกหลังส่งออกไฟล์แล้ว")
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *TeachingHandler) AddMakeup(c *fiber.Ctx) error {
	sectionID, err := uuid.Parse(c.Params("sectionId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid section id")
	}
	var m service.MakeupSchedule
	if err := c.BodyParser(&m); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Teaching.AddMakeup(c.Context(), UserID(c), sectionID, m); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *TeachingHandler) AddReviewDate(c *fiber.Ctx) error {
	sectionID, err := uuid.Parse(c.Params("sectionId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid section id")
	}
	var r service.LectureReview
	if err := c.BodyParser(&r); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Teaching.AddReviewDate(c.Context(), UserID(c), sectionID, r); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *TeachingHandler) ImportExcel(c *fiber.Ctx) error {
	termIDStr := c.FormValue("term_id")
	termID, err := uuid.Parse(termIDStr)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "term_id required")
	}
	fh, err := c.FormFile("file")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "file required")
	}
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	body, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	res, err := h.Svc.Teaching.ImportScheduleExcel(c.Context(), UserID(c), termID, fh.Filename, body)
	if err != nil {
		return err
	}
	return c.JSON(res)
}

func (h *TeachingHandler) Budget(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	snap, err := h.Svc.Budget.Compute(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(snap)
}

// -------------------- TA request --------------------

type TARequestHandler struct{ Svc *service.Container }

func (h *TARequestHandler) List(c *fiber.Ctx) error {
	if rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) && c.Query("pending") == "1" {
		out, err := h.Svc.TARequest.ListPending(c.Context())
		if err != nil {
			return err
		}
		return c.JSON(out)
	}
	uid := UserID(c)
	out, err := h.Svc.TARequest.ListForLecturer(c.Context(), uid)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *TARequestHandler) Create(c *fiber.Ctx) error {
	var in service.CreateTARequestInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	id, err := h.Svc.TARequest.Create(c.Context(), UserID(c), in)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": id})
}

func (h *TARequestHandler) Approve(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.TARequest.Approve(c.Context(), UserID(c), id); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *TARequestHandler) Reject(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.TARequest.Reject(c.Context(), UserID(c), id, body.Reason); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *TARequestHandler) ListWindows(c *fiber.Ctx) error {
	var termID *uuid.UUID
	if v := c.Query("term_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			termID = &id
		}
	}
	out, err := h.Svc.TARequest.ListWindows(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *TARequestHandler) UpsertWindow(c *fiber.Ctx) error {
	var w service.Window
	if err := c.BodyParser(&w); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	out, err := h.Svc.TARequest.UpsertWindow(c.Context(), UserID(c), w)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// -------------------- Docs --------------------

type DocsHandler struct{ Svc *service.Container }

func (h *DocsHandler) GetProfile(c *fiber.Ctx) error {
	p, err := h.Svc.Docs.GetProfile(c.Context(), UserID(c))
	if err != nil {
		return c.JSON(nil)
	}
	return c.JSON(p)
}

func (h *DocsHandler) UpsertProfile(c *fiber.Ctx) error {
	var p service.TAProfile
	if err := c.BodyParser(&p); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Docs.UpsertProfile(c.Context(), UserID(c), p); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *DocsHandler) ListDocs(c *fiber.Ctx) error {
	out, err := h.Svc.Docs.ListForUser(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *DocsHandler) UploadDoc(c *fiber.Ctx) error {
	kind := c.FormValue("kind")
	fh, err := c.FormFile("file")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "file required")
	}
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	id, err := h.Svc.Docs.Upload(c.Context(), UserID(c), kind, fh.Filename, fh.Header.Get("Content-Type"), fh.Size, src)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": id})
}

func (h *DocsHandler) CreditorForm(c *fiber.Ctx) error {
	body, name, err := h.Svc.Docs.BuildCreditorForm(c.Context(), UserID(c), h.Svc.Cfg.CreditorTemplatePath)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	c.Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	c.Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	return c.Send(body)
}

func (h *DocsHandler) Download(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	rc, filename, mime, err := h.Svc.Docs.OpenStored(c.Context(), id)
	if err != nil {
		return err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	c.Set("Content-Type", firstNonEmpty(mime, "application/octet-stream"))
	c.Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	return c.Send(body)
}

func (h *DocsHandler) ListPending(c *fiber.Ctx) error {
	out, err := h.Svc.Docs.ListReview(c.Context(), c.Query("status"))
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *DocsHandler) ListDocsForUser(c *fiber.Ctx) error {
	uid, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	docs, err := h.Svc.Docs.ListForUser(c.Context(), uid)
	if err != nil {
		return err
	}
	prof, _ := h.Svc.Docs.GetProfile(c.Context(), uid)
	return c.JSON(fiber.Map{
		"documents": docs,
		"profile":   prof,
	})
}

func (h *DocsHandler) ReviewProfile(c *fiber.Ctx) error {
	uid, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var body struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Docs.ReviewProfile(c.Context(), UserID(c), uid, body.Approve, body.Reason); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *DocsHandler) ReviewDoc(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var body struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Docs.Review(c.Context(), UserID(c), id, body.Approve, body.Reason); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(fiber.Map{"ok": true})
}

// -------------------- Workload / class schedule --------------------

type WorkloadHandler struct{ Svc *service.Container }

func (h *WorkloadHandler) ListClasses(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "term_id required")
	}
	out, err := h.Svc.Workload.ListClasses(c.Context(), UserID(c), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *WorkloadHandler) ReplaceClasses(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "term_id required")
	}
	var blocks []service.ClassBlock
	if err := c.BodyParser(&blocks); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Workload.ReplaceClasses(c.Context(), UserID(c), termID, blocks); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "count": len(blocks)})
}

// -------------------- Work logs --------------------

type WorkLogHandler struct{ Svc *service.Container }

func (h *WorkLogHandler) Generate(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.WorkLog.Generate(c.Context(), UserID(c), id)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *WorkLogHandler) List(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.WorkLog.List(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *WorkLogHandler) Upsert(c *fiber.Ctx) error {
	var w service.WorkLog
	if err := c.BodyParser(&w); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if w.AssignmentID == uuid.Nil {
		id, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid id")
		}
		w.AssignmentID = id
	}
	id, err := h.Svc.WorkLog.Upsert(c.Context(), UserID(c), w)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(fiber.Map{"id": id})
}

func (h *WorkLogHandler) Submit(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.WorkLog.Submit(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *WorkLogHandler) Approve(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.WorkLog.Approve(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *WorkLogHandler) Reject(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.WorkLog.Reject(c.Context(), UserID(c), id, body.Reason); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(fiber.Map{"ok": true})
}

// -------------------- Notify --------------------

type NotifyHandler struct{ Svc *service.Container }

func (h *NotifyHandler) List(c *fiber.Ctx) error {
	limit, _ := strconv.Atoi(c.Query("limit", "20"))
	out, err := h.Svc.Notify.List(c.Context(), UserID(c), limit)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *NotifyHandler) MarkRead(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.Notify.MarkRead(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// -------------------- Announce --------------------

type AnnounceHandler struct{ Svc *service.Container }

func (h *AnnounceHandler) List(c *fiber.Ctx) error {
	role := c.Query("role")
	if role == "" {
		if roles := Roles(c); len(roles) > 0 {
			role = roles[0]
		}
	}
	out, err := h.Svc.Announce.List(c.Context(), role)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *AnnounceHandler) Upsert(c *fiber.Ctx) error {
	var in service.Announcement
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	id, err := h.Svc.Announce.Upsert(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"id": id})
}

// -------------------- Dashboard --------------------

type DashboardHandler struct{ Svc *service.Container }

func (h *DashboardHandler) Executive(c *fiber.Ctx) error {
	var termID *uuid.UUID
	if v := c.Query("term_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			termID = &id
		}
	}
	out, err := h.Svc.Dashboard.Executive(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// -------------------- Export --------------------

type ExportHandler struct{ Svc *service.Container }

func (h *ExportHandler) CourseZip(c *fiber.Ctx) error {
	raw := strings.TrimSuffix(c.Params("id"), ".zip")
	id, err := uuid.Parse(raw)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	body, name, err := h.Svc.Export.BuildCourseZip(c.Context(), id)
	if err != nil {
		return err
	}
	// Freeze section edits — this export is now the source of truth for the
	// course's section list + student counts. Non-fatal if it fails; the
	// download still succeeds and staff can mark it manually if needed.
	_ = h.Svc.Teaching.MarkExported(c.Context(), id)
	c.Set("Content-Type", "application/zip")
	c.Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	return c.Send(body)
}

// -------------------- Audit --------------------

type AuditHandler struct{ Svc *service.Container }

func (h *AuditHandler) List(c *fiber.Ctx) error {
	limit, _ := strconv.Atoi(c.Query("limit", "100"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := h.Svc.Pool.Query(c.Context(), `
		SELECT id, at, actor_id, actor_role::text, action, entity, entity_id, ip::text, note
		FROM audit_logs ORDER BY at DESC LIMIT $1`, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []fiber.Map{}
	for rows.Next() {
		var id int64
		var at any
		var actorID *uuid.UUID
		var actorRole *string
		var action, entity string
		var entityID, ip, note *string
		if err := rows.Scan(&id, &at, &actorID, &actorRole, &action, &entity, &entityID, &ip, &note); err != nil {
			return err
		}
		out = append(out, fiber.Map{
			"id": id, "at": at, "actor_id": actorID, "actor_role": actorRole,
			"action": action, "entity": entity, "entity_id": entityID, "ip": ip, "note": note,
		})
	}
	return c.JSON(out)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
