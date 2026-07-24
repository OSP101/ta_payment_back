package handler

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/rbac"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/watermark"
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

func (h *UserHandler) Activate(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.Users.Activate(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// -------------------- Course --------------------

type CourseHandler struct{ Svc *service.Container }

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

// ListMyAssignments returns the assignment rows (one per section) held by the
// current TA. Optional ?teaching_course_id= narrows to a single course — used by
// the per-course TA view to resolve the assignment_id for /assignments/:id/worklog.
func (h *TeachingHandler) ListMyAssignments(c *fiber.Ctx) error {
	taID := UserID(c)
	var tcID *uuid.UUID
	if v := c.Query("teaching_course_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			tcID = &id
		}
	}
	out, err := h.Svc.Teaching.ListAssignmentsForTA(c.Context(), taID, tcID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// ClassKinds — GET /class-kinds?term_id=… : the term's timetable with its
// lecture/lab labels, used by the TA .ics import to fill in ประเภท (the .ics
// itself has no such field).
func (h *TeachingHandler) ClassKinds(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "term_id is required")
	}
	out, err := h.Svc.Teaching.ClassKinds(c.Context(), termID)
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

// Delete removes a mistakenly-opened course (only when it has no downstream
// records — the service enforces the guard).
func (h *TeachingHandler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.Teaching.Delete(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
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

// ReplaceSectionSchedules writes the full weekly schedule for one section.
// Body: [{ kind, day_of_week, start_time, end_time, room? }, ...]
// Empty array clears the schedule entirely.
func (h *TeachingHandler) ReplaceSectionSchedules(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	sectionID, err := uuid.Parse(c.Params("sectionId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid section id")
	}
	var body struct {
		Schedules []service.SectionSchedule `json:"schedules"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Teaching.ReplaceSectionSchedules(c.Context(), UserID(c), tcID, sectionID, body.Schedules); err != nil {
		if errors.Is(err, service.ErrCourseLocked) {
			return fiber.NewError(fiber.StatusConflict, "รายวิชานี้ถูกล็อกหลังส่งออกไฟล์แล้ว")
		}
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบ section")
		}
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
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

// DeleteMakeup removes a filed makeup for the section. Service enforces that
// submitted/approved worklog rows on the vanishing date block the delete.
func (h *TeachingHandler) DeleteMakeup(c *fiber.Ctx) error {
	sectionID, err := uuid.Parse(c.Params("sectionId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid section id")
	}
	makeupID, err := uuid.Parse(c.Params("makeupId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid makeup id")
	}
	if err := h.Svc.Teaching.DeleteMakeup(c.Context(), UserID(c), sectionID, makeupID); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// HolidayImpacts is the TA/lecturer per-course join: which holidays hit a
// scheduled class day, and did the lecturer file a makeup? Open to anyone who
// can already read the course (TA on the course, lecturer, staff/admin) — the
// service authz is handled implicitly since the shape of the response mirrors
// section_schedules that the reader can already fetch.
func (h *TeachingHandler) HolidayImpacts(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.Holiday.ImpactsForCourse(c.Context(), tcID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// RemindLecturerAboutMakeup — TA nudges the course's lecturer(s) that a
// holiday still needs a makeup date. Throttled 1/24h per (TA, course, date).
func (h *TeachingHandler) RemindLecturerAboutMakeup(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	originalDate := c.Params("originalDate")
	var body struct {
		Note string `json:"note"`
	}
	// Empty body is fine — note is optional. Ignore parse errors that come
	// from an empty body.
	_ = c.BodyParser(&body)
	if err := h.Svc.Holiday.RemindLecturer(c.Context(), UserID(c), tcID, originalDate, body.Note); err != nil {
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
	// Preview path: staff uploads to see per-course preview (new / existing /
	// missing_catalog / unmatched_officer) without touching the DB.
	if c.Query("dry_run") == "1" {
		preview, err := h.Svc.Teaching.PreviewImport(c.Context(), UserID(c), termID, fh.Filename, body)
		if err != nil {
			return err
		}
		return c.JSON(preview)
	}
	// Commit path: skip_codes is a comma-separated list of course codes the
	// staff has explicitly opted out of (e.g. unmatched-officer courses they
	// chose to skip rather than proceed unassigned).
	var skipCodes []string
	if raw := c.FormValue("skip_codes"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				skipCodes = append(skipCodes, p)
			}
		}
	}
	res, err := h.Svc.Teaching.CommitImport(c.Context(), UserID(c), termID, fh.Filename, body, skipCodes)
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
	isStaff := rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff)
	if isStaff {
		// Staff sees the full history by default so decided requests remain
		// visible; ?scope=pending (or legacy ?pending=1) narrows to submitted.
		if c.Query("scope") == "pending" || c.Query("pending") == "1" {
			out, err := h.Svc.TARequest.ListPending(c.Context())
			if err != nil {
				return err
			}
			return c.JSON(out)
		}
		out, err := h.Svc.TARequest.ListAll(c.Context())
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

func (h *TARequestHandler) Detail(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.TARequest.Detail(c.Context(), id)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, err.Error())
	}
	// Staff/admin can view anything; lecturers only their own requests.
	if !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		var owner uuid.UUID
		if err := h.Svc.Pool.QueryRow(c.Context(),
			`SELECT lecturer_id FROM ta_requests WHERE id = $1`, id).Scan(&owner); err != nil || owner != UserID(c) {
			return fiber.NewError(fiber.StatusForbidden, "forbidden")
		}
	}
	return c.JSON(out)
}

func (h *TARequestHandler) Create(c *fiber.Ctx) error {
	var in service.CreateTARequestInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	res, err := h.Svc.TARequest.Create(c.Context(), UserID(c), in)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.Status(fiber.StatusCreated).JSON(res)
}

// PreviewConflicts previews the schedule-conflict verdict for a TA against
// every section of a teaching course. Used by the lecturer's request form to
// flag conflicts inline at TA-picking time (instead of at submit time).
func (h *TARequestHandler) PreviewConflicts(c *fiber.Ctx) error {
	taID, err := uuid.Parse(c.Query("ta_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid ta_id")
	}
	tcID, err := uuid.Parse(c.Query("teaching_course_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid teaching_course_id")
	}
	out, err := h.Svc.TARequest.PreviewConflicts(c.Context(), taID, tcID)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, err.Error())
	}
	return c.JSON(fiber.Map{"conflicts": out})
}

// Candidates lists selectable TAs for a course's request form, each with their
// approved-course count + at-quota flag so the picker can exclude full TAs.
func (h *TARequestHandler) Candidates(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Query("teaching_course_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid teaching_course_id")
	}
	out, err := h.Svc.TARequest.Candidates(c.Context(), tcID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": out})
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

func (h *TARequestHandler) DeleteWindow(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.TARequest.DeleteWindow(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
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
		return err
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

// CreditorFormPDF renders the filled creditor-form PDF inline so the TA can
// preview it in the browser (<iframe>) before confirming attachment. Any
// authenticated caller may pass ?grid=1 to overlay a calibration grid on
// their own preview — the grid draws over the caller's own filled data, so
// it's not an information leak; it just makes on-form measurement possible.
func (h *DocsHandler) CreditorFormPDF(c *fiber.Ctx) error {
	grid := c.Query("grid") == "1"
	body, name, err := h.Svc.Docs.BuildCreditorFormPDF(c.Context(), UserID(c),
		h.Svc.Cfg.CreditorTemplatePath, h.Svc.Cfg.FontDir, grid)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	c.Set("Content-Type", "application/pdf")
	c.Set("Content-Disposition", "inline; filename=\""+name+"\"")
	c.Set("X-Content-Type-Options", "nosniff")
	// Never let a browser or intermediary cache a PDF containing PII.
	c.Set("Cache-Control", "no-store")
	return c.Send(body)
}

// ConfirmCreditorForm generates the PDF server-side and attaches it as the
// TA's creditor_form document (superseding any prior version), so the review
// pipeline treats it identically to a manually-uploaded file.
func (h *DocsHandler) ConfirmCreditorForm(c *fiber.Ctx) error {
	id, err := h.Svc.Docs.AttachGeneratedCreditorForm(c.Context(), UserID(c),
		h.Svc.Cfg.CreditorTemplatePath, h.Svc.Cfg.FontDir)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": id})
}

// BlankCreditorForm serves the untouched PDF template so a TA who doesn't
// trust the overlay can print, fill by hand, and upload manually. No PII;
// safe to serve without cache-control headers.
func (h *DocsHandler) BlankCreditorForm(c *fiber.Ctx) error {
	c.Set("Content-Type", "application/pdf")
	c.Set("Content-Disposition", "attachment; filename=\"creditor_form_blank.pdf\"")
	return c.SendFile(h.Svc.Cfg.CreditorTemplatePath)
}

func (h *DocsHandler) Download(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	// Retention-policy guard: if the file has been purged from disk (7 days
	// past approval), fail fast with 410 before trying to open a stale path.
	deleted, err := h.Svc.Docs.IsFileDeleted(c.Context(), id)
	if err != nil {
		return err
	}
	if deleted {
		return fiber.NewError(fiber.StatusGone, "ไฟล์ถูกลบตามนโยบายเก็บรักษา 7 วัน")
	}
	rc, filename, mime, ownerID, err := h.Svc.Docs.OpenStored(c.Context(), id)
	if err != nil {
		return err
	}
	defer rc.Close()

	// Only the owning TA, staff, or admin may fetch the file. Lecturers do
	// not review documents in this workflow and are blocked to keep PDPA
	// exposure minimal.
	caller := UserID(c)
	if caller != ownerID && !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		return fiber.NewError(fiber.StatusForbidden, "forbidden")
	}

	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	// Inline preview is fine for staff review; browsers will download
	// anything they can't render. `Content-Disposition: inline` gives them
	// the choice while still setting a filename for downloads.
	c.Set("Content-Type", firstNonEmpty(mime, "application/octet-stream"))
	c.Set("Content-Disposition", "inline; filename=\""+filename+"\"")
	c.Set("X-Content-Type-Options", "nosniff")
	return c.Send(body)
}

// History returns the full submission trail for a TA (all profile snapshots +
// all documents including superseded ones). Staff-only.
func (h *DocsHandler) History(c *fiber.Ctx) error {
	uid, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	hist, err := h.Svc.Docs.GetHistory(c.Context(), uid)
	if err != nil {
		return err
	}
	return c.JSON(hist)
}

// SelfHistory returns the caller's own submission history. Lets a TA review
// what they've submitted across rounds without exposing it to peers.
func (h *DocsHandler) SelfHistory(c *fiber.Ctx) error {
	hist, err := h.Svc.Docs.GetHistory(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(hist)
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
		return err
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
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// ApproveAll approves all three required docs + the profile in one shot and
// returns a one-shot ZIP-download token so the FE can trigger auto-download
// via same-tab navigation (no popup blocker).
func (h *DocsHandler) ApproveAll(c *fiber.Ctx) error {
	uid, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	res, err := h.Svc.Docs.ApproveAll(c.Context(), UserID(c), uid)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"ok":            true,
		"approved_docs": res.ApprovedDocIDs,
		"zip_token":     res.ZipToken,
	})
}

// RejectBatch rejects a list of {doc_id, reason} entries in a single tx and
// flips the profile to needs_fix so the TA can re-upload just the flagged
// files. Reasons are required per file.
func (h *DocsHandler) RejectBatch(c *fiber.Ctx) error {
	uid, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var body struct {
		Items []service.RejectItem `json:"items"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Docs.RejectBatch(c.Context(), UserID(c), uid, body.Items); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// MintZipToken lets an officer re-download the approved-docs ZIP later from
// the "approved" bucket in the review UI. Fresh token per call, TTL 60s.
// Officer must re-confirm their password because the ZIP contains PII
// (national ID + bank account) and this endpoint is reachable long after
// the original approval action.
func (h *DocsHandler) MintZipToken(c *fiber.Ctx) error {
	uid, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	token, err := h.Svc.Docs.MintZipToken(c.Context(), UserID(c), uid, body.Password)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"zip_token": token})
}

// DownloadZip consumes the one-shot token minted by ApproveAll (or
// MintZipToken) and streams a ZIP of the three approved documents. The zip
// contents are the raw originals — no watermark — because this is the audit
// copy the officer keeps offline.
func (h *DocsHandler) DownloadZip(c *fiber.Ctx) error {
	uid, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	token := c.Query("token")
	if token == "" {
		return fiber.NewError(fiber.StatusBadRequest, "token required")
	}
	docIDs, err := h.Svc.Docs.ConsumeZipToken(token, UserID(c), uid)
	if err != nil {
		return err
	}
	body, name, err := h.Svc.Docs.BuildDocsZip(c.Context(), docIDs)
	if err != nil {
		return err
	}
	c.Set("Content-Type", "application/zip")
	c.Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	c.Set("Cache-Control", "no-store")
	return c.Send(body)
}

// PreviewWatermarked serves a doc file with an officer-identifying watermark
// baked into the render, so a screenshot or download-from-preview leaks the
// officer's identity. Download endpoint (audit copy) is unwatermarked.
func (h *DocsHandler) PreviewWatermarked(c *fiber.Ctx) error {
	uid, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	docID, err := uuid.Parse(c.Params("docId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid doc id")
	}
	meta, err := h.Svc.Docs.LoadPreviewMeta(c.Context(), docID)
	if err != nil {
		return err
	}
	if meta.OwnerID != uid {
		return fiber.NewError(fiber.StatusBadRequest, "doc does not belong to user")
	}
	if meta.FileDeletedAt != nil {
		return fiber.NewError(fiber.StatusGone, "ไฟล์ถูกลบตามนโยบายเก็บรักษา 7 วัน")
	}
	rc, err := h.Svc.Docs.OpenByKey(meta.StorageKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	email, err := h.Svc.Docs.LookupEmail(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	text := email + " | " + time.Now().Format("2006-01-02 15:04")
	if meta.Superseded {
		text = "SUPERSEDED (round " + strconv.Itoa(meta.Round) + ") | " + text
	}
	stamped, outMime, err := watermark.Apply(body, meta.MIME, text)
	if err != nil {
		return err
	}
	c.Set("Content-Type", outMime)
	c.Set("Content-Disposition", "inline; filename=\""+meta.Filename+"\"")
	c.Set("Cache-Control", "private, no-store")
	c.Set("X-Content-Type-Options", "nosniff")
	return c.Send(stamped)
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
	privileged := rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff)
	out, err := h.Svc.WorkLog.List(c.Context(), UserID(c), id, privileged)
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
	// Always trust the URL param for the assignment id — never the body — so a
	// TA cannot target another TA's assignment by forging the payload.
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	w.AssignmentID = id
	wid, err := h.Svc.WorkLog.Upsert(c.Context(), UserID(c), w)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"id": wid})
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
	// year_month ("YYYY-MM") scopes the decision to one month; absent = all
	// submitted rows (legacy behaviour, still used by staff bulk tools).
	var body struct {
		YearMonth string `json:"year_month"`
	}
	_ = c.BodyParser(&body)
	privileged := rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff)
	if err := h.Svc.WorkLog.Approve(c.Context(), UserID(c), id, body.YearMonth, privileged); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *WorkLogHandler) PendingReports(c *fiber.Ctx) error {
	privileged := rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff)
	out, err := h.Svc.WorkLog.ListPending(c.Context(), UserID(c), privileged)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// ApprovalHistory — GET /teaching-courses/:id/approval-history — the
// lecturer's own approve/reject actions for a single course, newest first.
// Backs the "ประวัติการอนุมัติ" panel on the reports page.
func (h *WorkLogHandler) ApprovalHistory(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.WorkLog.ListApprovalHistory(c.Context(), UserID(c), tcID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// StaffListByCourse — GET /staff/courses/:tcId/worklogs — every TA's entries.
func (h *WorkLogHandler) StaffListByCourse(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.WorkLog.StaffListByCourse(c.Context(), tcID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// StaffListAssignments — GET /staff/courses/:tcId/assignments — approved
// (TA × section) slots for the editor's add-row picker.
func (h *WorkLogHandler) StaffListAssignments(c *fiber.Ctx) error {
	tcID, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.WorkLog.StaffListAssignments(c.Context(), tcID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// StaffUpsert — PUT /staff/worklogs — edit or add a row on any TA's behalf.
func (h *WorkLogHandler) StaffUpsert(c *fiber.Ctx) error {
	var w service.WorkLog
	if err := c.BodyParser(&w); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	id, err := h.Svc.WorkLog.StaffUpsert(c.Context(), UserID(c), w)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"id": id})
}

// StaffDelete — DELETE /staff/worklogs/:id — remove a draft/rejected row.
// Delete removes a single work_log row owned by the calling TA. The URL uses
// the log id under the /assignments prefix so the same collection namespace is
// reused; the handler ignores the :id param and trusts the log's own owner
// check in the service layer.
func (h *WorkLogHandler) Delete(c *fiber.Ctx) error {
	logID, err := uuid.Parse(c.Params("logId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid log id")
	}
	if err := h.Svc.WorkLog.Delete(c.Context(), UserID(c), logID); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *WorkLogHandler) StaffDelete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.WorkLog.StaffDelete(c.Context(), UserID(c), id); err != nil {
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
		Reason    string `json:"reason"`
		YearMonth string `json:"year_month"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	privileged := rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff)
	if err := h.Svc.WorkLog.Reject(c.Context(), UserID(c), id, body.Reason, body.YearMonth, privileged); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// -------------------- TA review schedules --------------------

// ListTAReviewSchedules — GET /assignments/:id/review-schedules
func (h *WorkLogHandler) ListTAReviewSchedules(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.WorkLog.ListTAReviewSchedules(c.Context(), UserID(c), id)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// ScheduleBusy — GET /assignments/:id/schedule-busy — the TA's occupied weekly
// slots (class timetable + teaching + review) for the review-editor overlap preview.
func (h *WorkLogHandler) ScheduleBusy(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.WorkLog.ScheduleBusyBlocks(c.Context(), UserID(c), id)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// AddTAReviewSchedule — POST /assignments/:id/review-schedules
func (h *WorkLogHandler) AddTAReviewSchedule(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var in service.TAReviewScheduleInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	rsID, err := h.Svc.WorkLog.AddTAReviewSchedule(c.Context(), UserID(c), id, in)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"id": rsID})
}

// UpdateTAReviewSchedule — PATCH /assignments/:id/review-schedules/:rsId
func (h *WorkLogHandler) UpdateTAReviewSchedule(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	rsID, err := uuid.Parse(c.Params("rsId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid rs id")
	}
	var in service.TAReviewScheduleInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.WorkLog.UpdateTAReviewSchedule(c.Context(), UserID(c), id, rsID, in); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// DeleteTAReviewSchedule — DELETE /assignments/:id/review-schedules/:rsId
func (h *WorkLogHandler) DeleteTAReviewSchedule(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	rsID, err := uuid.Parse(c.Params("rsId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid rs id")
	}
	if err := h.Svc.WorkLog.DeleteTAReviewSchedule(c.Context(), UserID(c), id, rsID); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// -------------------- Notify --------------------

type NotifyHandler struct{ Svc *service.Container }

func (h *NotifyHandler) List(c *fiber.Ctx) error {
	limit, _ := strconv.Atoi(c.Query("limit", "20"))
	unreadOnly := c.Query("unread") == "1" || c.Query("unread") == "true"
	out, err := h.Svc.Notify.List(c.Context(), UserID(c), limit, unreadOnly)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *NotifyHandler) UnreadCount(c *fiber.Ctx) error {
	n, err := h.Svc.Notify.UnreadCount(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"count": n})
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

func (h *NotifyHandler) MarkAllRead(c *fiber.Ctx) error {
	n, err := h.Svc.Notify.MarkAllRead(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "count": n})
}

// -------------------- Announce --------------------

type AnnounceHandler struct{ Svc *service.Container }

// List branches on caller role: staff/admin get every row (drafts + scheduled
// + expired) so the composer can act on them; everyone else sees only the
// live window for their own role.
func (h *AnnounceHandler) List(c *fiber.Ctx) error {
	roles := Roles(c)
	isStaff := rbac.Has(roles, rbac.RoleAdmin, rbac.RoleStaff)
	scope := c.Query("scope")
	includeAll := isStaff && (scope == "" || scope == "all")

	role := c.Query("role")
	if !isStaff {
		// Non-staff always filter to their own primary role; the query param
		// is ignored so a curious client can't peek at another audience.
		if len(roles) > 0 {
			role = roles[0]
		}
	}
	if !includeAll && role == "" && len(roles) > 0 {
		role = roles[0]
	}
	out, err := h.Svc.Announce.List(c.Context(), service.ListFilter{
		RoleFilter: role,
		IncludeAll: includeAll,
	})
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// Get returns one announcement. Non-staff callers can only see it if the row
// is live for their role — this makes /announcements/:id direct links safe.
func (h *AnnounceHandler) Get(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	a, err := h.Svc.Announce.Get(c.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบประกาศ")
		}
		return err
	}
	if !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		if a.Status != "live" {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบประกาศ")
		}
		roles := Roles(c)
		matched := false
		for _, want := range a.Audience {
			for _, have := range roles {
				if want == have {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			return fiber.NewError(fiber.StatusForbidden, "forbidden")
		}
	}
	return c.JSON(a)
}

func (h *AnnounceHandler) Upsert(c *fiber.Ctx) error {
	var in service.UpsertInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	id, err := h.Svc.Announce.Upsert(c.Context(), UserID(c), in)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบประกาศ")
		}
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(fiber.Map{"id": id})
}

func (h *AnnounceHandler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.Announce.Delete(c.Context(), UserID(c), id); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบประกาศ")
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *AnnounceHandler) Publish(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.Announce.Publish(c.Context(), UserID(c), id); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบประกาศ")
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *AnnounceHandler) Unpublish(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.Announce.Unpublish(c.Context(), UserID(c), id); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบประกาศ")
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// Allowed cover-image content types. WebP is included because the FE resizer
// can output it to keep upload sizes small on modern browsers.
var announceImageMIME = map[string]string{
	"image/jpeg": ".jpg",
	"image/jpg":  ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

// Hard cap on upload size. Bigger than that and we return 413 instead of
// silently storing megabytes of PII-adjacent binary. FE targets ~1600x900
// after resize, which comfortably fits under 4 MB even for photography.
const announceImageMaxBytes = 5 * 1024 * 1024

// UploadImage accepts a cover image, validates MIME + magic bytes + size,
// and stores it via the shared encrypted-at-rest store. Returns the key so
// the composer can attach it to the announcement.
func (h *AnnounceHandler) UploadImage(c *fiber.Ctx) error {
	fh, err := c.FormFile("file")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "file required")
	}
	if fh.Size > announceImageMaxBytes {
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
	defer src.Close()

	// Sniff the first bytes to confirm MIME. Header-only trust is a common
	// upload attack vector; verifying the magic makes a client-lied MIME
	// harmless.
	head := make([]byte, 12)
	n, _ := src.Read(head)
	if !imageMagicMatches(head[:n], ext) {
		return fiber.NewError(fiber.StatusUnsupportedMediaType, "ประเภทไฟล์ไม่ตรงกับเนื้อหา")
	}
	// Rewind — the sniff consumed the head bytes.
	if _, err := src.Seek(0, 0); err != nil {
		return err
	}

	// Preserve the true extension based on the sniffed MIME rather than the
	// (possibly forged) filename.
	stored := uuid.New().String() + ext
	key, size, err := h.Svc.Storage.Save("announcements", stored, src)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"key":  key,
		"size": size,
		"url":  "/api/v1/announcements/images/" + key,
	})
}

// ServeImage streams a stored cover image back through the same auth wall
// as the rest of the API. Cache is public within the session — same image
// per announcement is stable enough that a small cache saves round trips.
func (h *AnnounceHandler) ServeImage(c *fiber.Ctx) error {
	key := c.Params("*")
	if key == "" {
		return fiber.NewError(fiber.StatusBadRequest, "key required")
	}
	// Reject any traversal attempt; the stored layout is strictly
	// "announcements/YYYY/MM/DD/<uuid>.ext[.enc]".
	if strings.Contains(key, "..") || !strings.HasPrefix(key, "announcements/") {
		return fiber.NewError(fiber.StatusBadRequest, "invalid key")
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
	// Content-Type from stored extension. Strip the encryption suffix.
	k := strings.TrimSuffix(key, ".enc")
	switch strings.ToLower(k[strings.LastIndex(k, "."):]) {
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

// imageMagicMatches sniffs the first bytes of an upload and returns true if
// they match the expected format. Keeps the check tight — no autodetection
// fallback, so a corrupted or mislabeled upload never lands.
func imageMagicMatches(head []byte, ext string) bool {
	switch ext {
	case ".jpg", ".jpeg":
		return len(head) >= 3 && head[0] == 0xff && head[1] == 0xd8 && head[2] == 0xff
	case ".png":
		return len(head) >= 8 &&
			head[0] == 0x89 && head[1] == 0x50 && head[2] == 0x4e && head[3] == 0x47 &&
			head[4] == 0x0d && head[5] == 0x0a && head[6] == 0x1a && head[7] == 0x0a
	case ".webp":
		return len(head) >= 12 &&
			string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP"
	}
	return false
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

// TaOverview — GET /dashboard/ta/me — TA's per-course status + estimated pay.
func (h *DashboardHandler) TaOverview(c *fiber.Ctx) error {
	out, err := h.Svc.Dashboard.TaOverview(c.Context(), UserID(c))
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// LecturerOverview — GET /dashboard/lecturer/me — per-course TA count, hours, budget.
func (h *DashboardHandler) LecturerOverview(c *fiber.Ctx) error {
	var termID *uuid.UUID
	if v := c.Query("term_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			termID = &id
		}
	}
	out, err := h.Svc.Dashboard.LecturerOverview(c.Context(), UserID(c), termID, h.Svc.Budget)
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
	body, name, taCount, err := h.Svc.Export.BuildCourseZip(c.Context(), id)
	if err != nil {
		return err
	}
	actor := UserID(c)
	// Lock every fully-approved (TA × month) in the course BEFORE the file leaves
	// the server: downloading the ZIP IS the freeze point for the payout numbers
	// (no digital signatures). This MUST NOT be best-effort — if the lock fails
	// and we still shipped the file, finance would hold a payout document whose
	// underlying worklogs stayed editable (file silently diverges from the DB).
	// Fail the request instead so staff retries; months still being worked on
	// stay editable and lock on a later re-export.
	if _, err := h.Svc.SubmissionPeriods.MarkCourseExported(c.Context(), actor, id); err != nil {
		return err
	}
	// Freeze section edits — this export is now the source of truth for the
	// course's section list + student counts. Non-fatal if it fails; the numbers
	// are already locked above and staff can mark sections manually if needed.
	_ = h.Svc.Teaching.MarkExported(c.Context(), id)
	// The export contains national IDs + bank details — audit who pulled it and
	// when (PII access trail, M5).
	h.Svc.Auditor.Log(c.Context(), audit.Entry{ActorID: &actor, Action: "export.course", Entity: "teaching_course", EntityID: id.String(), IP: c.IP(), UserAgent: c.Get("User-Agent")})

	// Persist the batch so the dashboard can list history + budget snapshot.
	// TotalBaht is the ACTUAL paid sum (Σ actual_paid after the pro-rata cap) so
	// the recorded figure matches the ZIP the staff hands to finance — the old
	// Budget.UsedBaht used different math and never reconciled with the file.
	// Best-effort — a DB write failure must NOT hide the (already-locked) zip.
	if prev, perr := h.Svc.Export.CoursePreview(c.Context(), id); perr == nil {
		_, _ = h.Svc.ExportBatches.Record(c.Context(), actor, service.ExportBatch{
			TeachingCourseID: id,
			FilePath:         name, // in-memory zip; we persist just the name for reference
			FileName:         name,
			TACount:          taCount,
			TotalBaht:        prev.TotalActual,
		})
	}

	c.Set("Content-Type", "application/zip")
	c.Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	return c.Send(body)
}

// CoursesSummary powers the exports dashboard: budget/usage/pending months per
// course, filtered by ?term_id=.
func (h *ExportHandler) CoursesSummary(c *fiber.Ctx) error {
	var termID uuid.UUID
	if raw := c.Query("term_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid term_id")
		}
		termID = id
	}
	out, err := h.Svc.ExportBatches.DashboardSummary(c.Context(), h.Svc.Budget, termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// CoursePreview returns the read-only payout preview (per-TA numbers + profile
// readiness) so staff can review before the locking download. No side effects.
func (h *ExportHandler) CoursePreview(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.Export.CoursePreview(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// CourseHistory returns the export batches for a single course, newest first.
func (h *ExportHandler) CourseHistory(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.ExportBatches.ListByCourse(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// AppointmentOrder — POST /exports/appointment-order — returns ZIP with PDF+DOCX.
func (h *ExportHandler) AppointmentOrder(c *fiber.Ctx) error {
	var in service.AppointmentOrderInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	body, name, err := h.Svc.Appointment.Build(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, "application/zip")
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+name+`"`)
	return c.Send(body)
}

func (h *ExportHandler) UnlockCourse(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.Teaching.Unexport(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
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

// -------------------- Admin officers (executive roster) --------------------

type AdminOfficerHandler struct{ Svc *service.Container }

func (h *AdminOfficerHandler) List(c *fiber.Ctx) error {
	includeInactive := c.Query("include_inactive") == "1"
	out, err := h.Svc.AdminOfficers.List(c.Context(), includeInactive)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *AdminOfficerHandler) Upsert(c *fiber.Ctx) error {
	var in service.AdminOfficer
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	out, err := h.Svc.AdminOfficers.Upsert(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *AdminOfficerHandler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.AdminOfficers.Delete(c.Context(), UserID(c), id); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "admin officer not found")
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// ============================================================================
// HolidayHandler — staff/admin CRUD for the public_holidays table.
// ============================================================================

type HolidayHandler struct{ Svc *service.Container }

// List returns holidays, optionally filtered to one calendar year via ?year=.
// Any authenticated user may read (TA/lecturer UIs surface the list too), but
// mutations are gated at the router.
func (h *HolidayHandler) List(c *fiber.Ctx) error {
	year := 0
	if v := c.Query("year"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1900 && n <= 3000 {
			year = n
		}
	}
	out, err := h.Svc.Holiday.List(c.Context(), year)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *HolidayHandler) Create(c *fiber.Ctx) error {
	var in service.HolidayInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	id, err := h.Svc.Holiday.Create(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": id})
}

func (h *HolidayHandler) BulkCreate(c *fiber.Ctx) error {
	var in []service.HolidayInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	inserted, err := h.Svc.Holiday.BulkCreate(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"inserted": inserted, "total": len(in)})
}

func (h *HolidayHandler) Patch(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var body struct {
		NameTH string  `json:"name_th"`
		NameEN *string `json:"name_en,omitempty"`
		Note   *string `json:"note,omitempty"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	if err := h.Svc.Holiday.Patch(c.Context(), UserID(c), id, body.NameTH, body.NameEN, body.Note); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

func (h *HolidayHandler) Delete(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.Holiday.Delete(c.Context(), UserID(c), id); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// SyncFromBOT pulls national holidays from BOT (Bank of Thailand) Open API.
// Two invocation modes (?start_year+?end_year takes priority):
//   - ?year=YYYY                              → single-year, returns SyncFromBOTResult
//   - ?start_year=YYYY&end_year=YYYY          → range, returns SyncFromBOTRangeResult
//
// Requires BOT_API_CLIENT_ID in server env.
func (h *HolidayHandler) SyncFromBOT(c *fiber.Ctx) error {
	startS, endS := c.Query("start_year"), c.Query("end_year")
	if startS != "" || endS != "" {
		startY, err1 := strconv.Atoi(startS)
		endY, err2 := strconv.Atoi(endS)
		if err1 != nil || err2 != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid start_year/end_year")
		}
		res, err := h.Svc.Holiday.SyncFromBOTRange(c.Context(), UserID(c), startY, endY)
		if err != nil {
			return err
		}
		return c.JSON(res)
	}
	year, err := strconv.Atoi(c.Query("year"))
	if err != nil || year < 1900 || year > 3000 {
		return fiber.NewError(fiber.StatusBadRequest, "invalid year")
	}
	res, err := h.Svc.Holiday.SyncFromBOT(c.Context(), UserID(c), year)
	if err != nil {
		return err
	}
	return c.JSON(res)
}
