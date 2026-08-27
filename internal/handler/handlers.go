package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
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
	limit, _ := strconv.Atoi(c.Query("limit", "50"))
	offset, _ := strconv.Atoi(c.Query("offset", "0"))
	f := service.UserListFilter{
		Role:   c.Query("role"),
		Search: c.Query("q"),
		Status: c.Query("status"),
		Sort:   c.Query("sort"),
		Desc:   c.Query("dir") == "desc",
		Limit:  limit,
		Offset: offset,
	}
	// Lecturers may only list TA accounts (used by the "request TA" flow)
	if rbac.Has(Roles(c), rbac.RoleLecturer) && !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		if f.Role != rbac.RoleTA {
			return fiber.NewError(fiber.StatusForbidden, "lecturers may only list TA accounts")
		}
	}
	users, total, err := h.Svc.Users.List(c.Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": users, "total": total})
}

func (h *UserHandler) Create(c *fiber.Ctx) error {
	var in service.CreateUserInput
	if err := Bind(c, &in); err != nil {
		return err
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
	if err := Bind(c, &in); err != nil {
		return err
	}
	u, err := h.Svc.Users.Update(c.Context(), UserID(c), id, in)
	if err != nil {
		return err
	}
	// A role edit changes what the target's EXISTING token is allowed to do —
	// roles ride in the JWT claims (see Login), so the already-issued token
	// keeps the old roles until it expires unless the session behind it is
	// revoked. Only when roles were actually part of this request: this
	// handler also carries profile-field edits that have no such stake.
	if in.Roles != nil {
		if err := h.Svc.Sessions.RevokeAllForUser(c.Context(), id, "roles_changed"); err != nil {
			return err
		}
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
	// The old password's session(s) must not survive a reset — otherwise
	// whoever was already signed in keeps working under the OLD password's
	// session while the new temp password sits unused.
	if err := h.Svc.Sessions.RevokeAllForUser(c.Context(), id, "password_reset"); err != nil {
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
		ConfirmEmail string `json:"confirm_email" validate:"required"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
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
	// AccountGuard's live is_active read already blocks a deactivated user's
	// next request regardless of this — see middleware.go — but revoking here
	// too means the session row itself shows WHY (revoke_reason
	// "account_deactivated") instead of leaving that only inferable from the
	// users table's audit log.
	if err := h.Svc.Sessions.RevokeAllForUser(c.Context(), id, "account_deactivated"); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// UnlockPasswordGate clears the re-authentication lockout on a user who mistyped
// their password too many times at the document-download or worklog-edit gate.
//
// No confirm-email step, unlike Deactivate: this restores access rather than
// removing it, and the worst case of a misfired click is that someone who was
// not locked out stays not locked out. was_locked tells the admin which of those
// just happened.
func (h *UserHandler) UnlockPasswordGate(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	wasLocked, err := h.Svc.Users.ClearPasswordGateLockout(c.Context(), UserID(c), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "user not found")
		}
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "was_locked": wasLocked})
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

// -------------------- Enrollment --------------------

// EnrollmentHandler exposes a TA's education-level history — see
// internal/service/enrollment.go and migration 0094.
type EnrollmentHandler struct{ Svc *service.Container }

func (h *EnrollmentHandler) List(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	items, err := h.Svc.Enrollments.List(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": items})
}

// RecordTransition is staff/admin only (see the route gate in router.go) —
// closes the TA's current active enrollment period and opens a new one.
func (h *EnrollmentHandler) RecordTransition(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var in service.RecordTransitionInput
	if err := Bind(c, &in); err != nil {
		return err
	}
	e, err := h.Svc.Enrollments.RecordTransition(c.Context(), UserID(c), id, in)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "user not found")
		}
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(e)
}

// SetSessionScope is POST /me/enrollment-scope — self-only, no role gate
// beyond being authenticated: EnrollmentService.SetSessionScope's own
// ownership check is what actually protects this, the same way any other
// "/me/..." write only ever acts on the caller's own account.
func (h *EnrollmentHandler) SetSessionScope(c *fiber.Ctx) error {
	var in struct {
		EnrollmentID uuid.UUID `json:"enrollment_id" validate:"required"`
	}
	if err := Bind(c, &in); err != nil {
		return err
	}
	if err := h.Svc.Enrollments.SetSessionScope(c.Context(), UserID(c), SessionID(c), in.EnrollmentID); err != nil {
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
	if err := Bind(c, &in); err != nil {
		return err
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
	if err := Bind(c, &in); err != nil {
		return err
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
	if err := Bind(c, &in); err != nil {
		return err
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
// TimetableForm builds the faculty's signed weekly form for one TA and term —
// their own classes and every TA duty they hold, in one grid.
//
// A TA may only ask for their own; staff, admin and a lecturer may ask for
// anyone's, because the lecturer signs it and staff review against it.
// timetableFormTarget resolves whose form is being asked for, and whether the
// caller may see it. A TA gets their own; staff, admin and lecturers may name
// anyone. Shared by the JSON and PDF handlers so the two cannot drift into
// different permissions for the same document.
func (h *TeachingHandler) timetableFormTarget(c *fiber.Ctx) (taID, termID uuid.UUID, yearMonth string, err error) {
	taID = UserID(c)
	if raw := c.Query("user_id"); raw != "" {
		id, perr := uuid.Parse(raw)
		if perr != nil {
			return uuid.Nil, uuid.Nil, "", fiber.NewError(fiber.StatusBadRequest, "invalid user_id")
		}
		if id != taID {
			switch {
			case rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff):
				// Staff prints forms for anyone; that is the job.
			case rbac.Has(Roles(c), rbac.RoleLecturer):
				// A lecturer prints forms for THEIR TAs. Before this check any
				// lecturer account could pull any TA's weekly whereabouts by
				// iterating user ids — a personal timetable is not faculty-wide
				// data.
				ok, qerr := h.Svc.Teaching.LecturerSupervisesTA(c.Context(), UserID(c), id)
				if qerr != nil {
					return uuid.Nil, uuid.Nil, "", qerr
				}
				if !ok {
					return uuid.Nil, uuid.Nil, "", fiber.NewError(fiber.StatusForbidden, "forbidden")
				}
			default:
				return uuid.Nil, uuid.Nil, "", fiber.NewError(fiber.StatusForbidden, "forbidden")
			}
		}
		taID = id
	}
	termID, perr := uuid.Parse(c.Query("term_id"))
	if perr != nil {
		return uuid.Nil, uuid.Nil, "", fiber.NewError(fiber.StatusBadRequest, "term_id is required")
	}
	return taID, termID, c.Query("year_month"), nil
}
func (h *TeachingHandler) TimetableForm(c *fiber.Ctx) error {
	taID, termID, yearMonth, err := h.timetableFormTarget(c)
	if err != nil {
		return err
	}
	out, err := h.Svc.Teaching.BuildTimetableForm(c.Context(), taID, termID, yearMonth)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// TimetableFormPDF serves the same document as a real PDF, so it can be filed
// and signed without a browser in the loop.
func (h *TeachingHandler) TimetableFormPDF(c *fiber.Ctx) error {
	taID, termID, yearMonth, err := h.timetableFormTarget(c)
	if err != nil {
		return err
	}
	body, err := h.Svc.Teaching.BuildTimetableFormPDF(c.Context(), taID, termID, yearMonth)
	if errors.Is(err, service.ErrNoFontDir) {
		// A deployment gap, not a bad request: say so instead of 400/500.
		return fiber.NewError(fiber.StatusServiceUnavailable, err.Error())
	}
	if err != nil {
		return err
	}
	name := "ตารางปฏิบัติงาน-TA"
	if yearMonth != "" {
		name += "-" + yearMonth
	}
	c.Set("Content-Type", "application/pdf")
	c.Set("Content-Disposition", contentDisposition("inline", name+".pdf"))
	return c.Send(body)
}

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
	if err := Bind(c, &in); err != nil {
		return err
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
	// -1 means "no change" so callers can update one track without touching the other,
	// so the floor is -1 (the sentinel), not 0.
	body := struct {
		NumStudents        int `json:"num_students" validate:"gte=-1"`
		NumStudentsRegular int `json:"num_students_regular" validate:"gte=-1"`
		NumStudentsSpecial int `json:"num_students_special" validate:"gte=-1"`
	}{NumStudents: -1, NumStudentsRegular: -1, NumStudentsSpecial: -1}
	if err := Bind(c, &body); err != nil {
		return err
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
	if err := Bind(c, &in); err != nil {
		return err
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
	if err := Bind(c, &in); err != nil {
		return err
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
	if err := Bind(c, &in); err != nil {
		return err
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
	if err := Bind(c, &body); err != nil {
		return err
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
	if err := Bind(c, &m); err != nil {
		return err
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
	if err := Bind(c, &r); err != nil {
		return err
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
	// A lecturer may only read their own course's budget; staff/admin see
	// all. BudgetService.Compute itself takes no actor — it is also called
	// from several admin/staff-only aggregate views (dashboards, exports)
	// that legitimately span every course, so the ownership check belongs
	// here, at this specific lecturer-reachable route, not inside Compute.
	if !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		owns, err := h.Svc.Teaching.LecturerOwnsCourse(c.Context(), UserID(c), id)
		if err != nil {
			return err
		}
		if !owns {
			return fiber.NewError(fiber.StatusForbidden, "forbidden")
		}
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
	if err := Bind(c, &in); err != nil {
		return err
	}
	res, err := h.Svc.TARequest.Create(c.Context(), UserID(c), in)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.Status(fiber.StatusCreated).JSON(res)
}

// Cancel — POST /ta-requests/:id/cancel — withdraws the caller's own request.
// TARequestService.Cancel is the single source of truth for whether this is
// currently allowed (ownership, status, no work_logs yet); this handler does
// no additional gating of its own.
func (h *TARequestHandler) Cancel(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	if err := h.Svc.TARequest.Cancel(c.Context(), UserID(c), id); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.JSON(fiber.Map{"ok": true})
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
	// This route is lecturer-only (router.go), and PreviewConflicts itself
	// takes no actor — without this, any lecturer could probe another
	// course's schedule-conflict data for an arbitrary TA by teaching_course_id.
	if !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		owns, err := h.Svc.Teaching.LecturerOwnsCourse(c.Context(), UserID(c), tcID)
		if err != nil {
			return err
		}
		if !owns {
			return fiber.NewError(fiber.StatusForbidden, "forbidden")
		}
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
	// A lecturer may only browse candidates for their own course (names,
	// emails, approved-course counts) — staff/admin see all. Candidates
	// itself takes no actor, same reasoning as PreviewConflicts above.
	if !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		owns, err := h.Svc.Teaching.LecturerOwnsCourse(c.Context(), UserID(c), tcID)
		if err != nil {
			return err
		}
		if !owns {
			return fiber.NewError(fiber.StatusForbidden, "forbidden")
		}
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
	if err := Bind(c, &w); err != nil {
		return err
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
	if err := Bind(c, &p); err != nil {
		return err
	}
	if err := h.Svc.Docs.UpsertProfile(c.Context(), UserID(c), p); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// RecordPdpaConsent is called by the PdpaConsentModal on the frontend before
// UpsertProfile will accept a submission — see that function's own consent
// check for why this is not merely a UI nicety.
func (h *DocsHandler) RecordPdpaConsent(c *fiber.Ctx) error {
	if err := h.Svc.Docs.RecordPdpaConsent(c.Context(), UserID(c), c.IP(), c.Get("User-Agent")); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

type revealCitizenIDReq struct {
	Password string `json:"password" validate:"required"`
}

// RevealCitizenID lets a TA see their own full citizen ID on /account/my-data
// — the PDPA "view my data" access right. Password-gated (same step-up-auth
// pattern MFAHandler.Disable/RegenerateRecoveryCodes use) rather than free on
// any authenticated request, since it decrypts and returns the one field this
// codebase otherwise treats as write-only. Reuses RevealCitizenID's existing
// audited path with actor == target == the caller themselves.
func (h *DocsHandler) RevealCitizenID(c *fiber.Ctx) error {
	var in revealCitizenIDReq
	if err := Bind(c, &in); err != nil {
		return err
	}
	uid := UserID(c)
	if err := service.VerifyUserPassword(c.Context(), h.Svc.Pool, uid, in.Password); err != nil {
		return err
	}
	nationalID, err := h.Svc.Docs.RevealCitizenID(c.Context(), uid, uid, "self-service PDPA data access")
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"national_id": nationalID})
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
		// Return the error as-is so ErrorHandler can honour a UserError's own
		// status. Wrapping everything in StatusBadRequest flattened the
		// distinctions the upload path is careful to make: a virus detection
		// (422) and "the scanner is down, try again" (503) both arrived as 400,
		// so a client could not tell a rejected file from an outage.
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": id})
}

// CreditorFormPDF renders the filled creditor form from the posted payload so
// the TA can preview it in an <iframe> before confirming.
//
// POST, not GET, because the body carries the data: nothing sensitive is
// stored, so there is no server-side state to render from (see migration
// 0047). That also keeps the national ID and account number out of the URL,
// out of history, and out of access logs.
//
// ?grid=1 overlays a calibration grid on the caller's own preview — it draws
// over data the caller just submitted, so it leaks nothing.
func (h *DocsHandler) CreditorFormPDF(c *fiber.Ctx) error {
	var in service.TAProfile
	if err := Bind(c, &in); err != nil {
		return err
	}
	grid := c.Query("grid") == "1"
	body, name, err := h.Svc.Docs.BuildCreditorFormPDF(c.Context(), UserID(c), in,
		h.Svc.Cfg.CreditorTemplatePath, h.Svc.Cfg.FontDir, grid)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	c.Set("Content-Type", "application/pdf")
	c.Set("Content-Disposition", contentDisposition("inline", name))
	c.Set("X-Content-Type-Options", "nosniff")
	// Never let a browser or intermediary cache a PDF containing PII.
	c.Set("Cache-Control", "no-store")
	return c.Send(body)
}

// ConfirmCreditorForm renders the PDF from the posted payload and attaches it
// as the TA's creditor_form document (superseding any prior version), so the
// review pipeline treats it identically to a manually-uploaded file.
func (h *DocsHandler) ConfirmCreditorForm(c *fiber.Ctx) error {
	var in service.TAProfile
	if err := Bind(c, &in); err != nil {
		return err
	}
	id, err := h.Svc.Docs.AttachGeneratedCreditorForm(c.Context(), UserID(c), in,
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
	c.Set("Content-Disposition", contentDisposition("attachment", "creditor_form_blank.pdf"))
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
	// Audited only when someone OTHER than the document's own owner reads
	// it — an officer pulling up a TA's ID-card photo is exactly the "PII
	// read back out" event citizen_id.go's RevealCitizenID already treats as
	// worth a trail; a TA opening their own upload is not.
	if caller != ownerID {
		if err := h.Svc.Auditor.Log(c.Context(), audit.Entry{
			ActorID: &caller, Action: "ta_doc.view", Entity: "ta_document", EntityID: id.String(),
			IP: c.IP(), UserAgent: c.Get("User-Agent"),
		}); err != nil {
			return err
		}
	}

	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	// Inline preview is fine for staff review; browsers will download
	// anything they can't render. `Content-Disposition: inline` gives them
	// the choice while still setting a filename for downloads.
	c.Set("Content-Type", firstNonEmpty(mime, "application/octet-stream"))
	c.Set("Content-Disposition", contentDisposition("inline", filename))
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
		Reason  string `json:"reason" validate:"omitempty,max=500"`
	}
	if err := Bind(c, &body); err != nil {
		return err
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
		Reason  string `json:"reason" validate:"omitempty,max=500"`
	}
	if err := Bind(c, &body); err != nil {
		return err
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
		Items []service.RejectItem `json:"items" validate:"required,dive"`
	}
	if err := Bind(c, &body); err != nil {
		return err
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
// MintAllApprovedZipToken gates the bulk download behind the officer's password
// and returns how many TAs the resulting file will cover, so the UI can say so
// before the download starts.
//
// user_ids is required: the TAs the officer approved on the screen in front of
// them. See MintAllApprovedZipToken for why there is no "everyone" default.
func (h *DocsHandler) MintAllApprovedZipToken(c *fiber.Ctx) error {
	var body struct {
		Password string      `json:"password" validate:"required"`
		UserIDs  []uuid.UUID `json:"user_ids" validate:"required,dive,required"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
	token, taCount, err := h.Svc.Docs.MintAllApprovedZipToken(
		c.Context(), UserID(c), body.Password, body.UserIDs)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"zip_token": token, "ta_count": taCount})
}

// DownloadAllZip serves the bulk bundle. The token was minted against
// uuid.Nil, so ConsumeZipToken's owner binding only matches here — a bulk token
// cannot be redirected at the single-TA route, and vice versa.
func (h *DocsHandler) DownloadAllZip(c *fiber.Ctx) error {
	token := c.Query("token")
	if token == "" {
		return fiber.NewError(fiber.StatusBadRequest, "token required")
	}
	docIDs, err := h.Svc.Docs.ConsumeZipToken(token, UserID(c), uuid.Nil)
	if err != nil {
		return err
	}
	taCount := h.Svc.Docs.CountTAsInDocs(c.Context(), docIDs)
	// No ClaimZipDownload here: the bulk pull is exempt from the per-TA quota (it
	// would otherwise spend everyone's allowance at once).
	//
	// It IS recorded as a hand-off, which is a different thing from being counted —
	// BuildAllApprovedBundle does that itself, deliberately, so this route cannot
	// serve documents without recording that they left.
	body, name, err := h.Svc.Docs.BuildAllApprovedBundle(c.Context(), UserID(c), docIDs, taCount)
	if err != nil {
		return err
	}
	if strings.HasSuffix(name, ".pdf") {
		c.Set("Content-Type", "application/pdf")
	} else {
		c.Set("Content-Type", "application/zip")
	}
	c.Set("Content-Disposition", contentDisposition("attachment", name))
	c.Set("Cache-Control", "no-store")
	return c.Send(body)
}

func (h *DocsHandler) MintZipToken(c *fiber.Ctx) error {
	uid, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var body struct {
		Password string `json:"password" validate:"required"`
	}
	if err := Bind(c, &body); err != nil {
		return err
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
	// Spend one of the round's allowed downloads. After the build so a failed
	// build costs nothing, and before the send so the bytes never leave when
	// the allowance is gone.
	if err := h.Svc.Docs.ClaimZipDownload(c.Context(), UserID(c), uid); err != nil {
		return err
	}
	// The bundle is normally a single merged PDF now; it falls back to a ZIP
	// when a set contains a pre-PDF-only image. Derive the type from what was
	// actually produced — a merged PDF served as application/zip downloads as
	// an unopenable archive.
	if strings.HasSuffix(name, ".pdf") {
		c.Set("Content-Type", "application/pdf")
	} else {
		c.Set("Content-Type", "application/zip")
	}
	c.Set("Content-Disposition", contentDisposition("attachment", name))
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
	caller := UserID(c)
	// This route is adminOrStaff-only (router.go) — the visitor is always
	// someone other than the document's owner, so every call is a "PII read
	// back out" event in the same sense citizen_id.go's RevealCitizenID
	// already treats that way. The watermark below traces a LEAKED copy back
	// to the viewer; this traces the VIEW itself, queryable from the audit
	// log without needing the leaked file in hand.
	if err := h.Svc.Auditor.Log(c.Context(), audit.Entry{
		ActorID: &caller, Action: "ta_doc.view_watermarked", Entity: "ta_document", EntityID: docID.String(),
		IP: c.IP(), UserAgent: c.Get("User-Agent"),
	}); err != nil {
		return err
	}
	email, err := h.Svc.Docs.LookupEmail(c.Context(), caller)
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
	c.Set("Content-Disposition", contentDisposition("inline", meta.Filename))
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
	// The lock ships with the data so the page can render read-only instead of
	// letting the TA edit for a minute and then fail on save.
	reason, err := h.Svc.Workload.ScheduleLockedReason(c.Context(), UserID(c), termID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"blocks": out, "locked": reason != "", "lock_reason": reason})
}

func (h *WorkloadHandler) ReplaceClasses(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "term_id required")
	}
	// Bind doesn't apply here: the body is a bare JSON array, and
	// validator.Struct requires a struct (or pointer to one) — passed a slice
	// it returns InvalidValidationError, which Bind would surface as a
	// blanket "invalid body" for every request, valid or not.
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
	if err := Bind(c, &w); err != nil {
		return err
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
	// submitted rows (legacy behaviour, still used by staff bulk tools). The
	// parse error is intentionally swallowed — callers that send no body at
	// all (or the wrong content-type) still get that "all rows" behaviour, so
	// this can't become Bind: Bind would 400 on exactly the empty-body calls
	// this endpoint is meant to accept. Format is still checked downstream by
	// WorkLog.Approve's own validateYearMonth.
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

// ApproveBatch — POST /worklog/approve-batch — approve several assignments in
// ONE transaction. The lecturer's "อนุมัติทั้งคนนี้" covers a TA who helps with
// several sections; looping over the single-assignment endpoint left them
// half-approved whenever the second call was refused.
func (h *WorkLogHandler) ApproveBatch(c *fiber.Ctx) error {
	var body struct {
		AssignmentIDs []string `json:"assignment_ids" validate:"required,min=1,dive,uuid4"`
		YearMonth     string   `json:"year_month" validate:"omitempty,datetime=2006-01"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
	if len(body.AssignmentIDs) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "assignment_ids required")
	}
	// A cap on the batch: this is one transaction holding an advisory lock per
	// course, and an unbounded list would hold it for as long as the caller
	// cared to make it.
	if len(body.AssignmentIDs) > 100 {
		return fiber.NewError(fiber.StatusBadRequest, "assignment_ids: มากเกินไป (สูงสุด 100)")
	}
	ids := make([]uuid.UUID, 0, len(body.AssignmentIDs))
	for _, raw := range body.AssignmentIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid assignment id: "+raw)
		}
		ids = append(ids, id)
	}
	privileged := rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff)
	if err := h.Svc.WorkLog.ApproveMany(c.Context(), UserID(c), ids, body.YearMonth, privileged); err != nil {
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
	// A lecturer may only read their own course's rows; staff/admin see all.
	if !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		owns, err := h.Svc.Teaching.LecturerOwnsCourse(c.Context(), UserID(c), tcID)
		if err != nil {
			return err
		}
		if !owns {
			return fiber.NewError(fiber.StatusForbidden, "forbidden")
		}
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
	// A lecturer may only read their own course's rows; staff/admin see all.
	if !rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff) {
		owns, err := h.Svc.Teaching.LecturerOwnsCourse(c.Context(), UserID(c), tcID)
		if err != nil {
			return err
		}
		if !owns {
			return fiber.NewError(fiber.StatusForbidden, "forbidden")
		}
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
	if err := Bind(c, &w); err != nil {
		return err
	}
	// Staff/admin may edit any course; a lecturer only their own, enforced in
	// the service so the rule survives a future caller.
	privileged := rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff)
	id, err := h.Svc.WorkLog.StaffUpsert(c.Context(), UserID(c), privileged, w)
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
	privileged := rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff)
	if err := h.Svc.WorkLog.StaffDelete(c.Context(), UserID(c), privileged, id); err != nil {
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
		Reason    string `json:"reason" validate:"required,max=500"`
		YearMonth string `json:"year_month" validate:"omitempty,datetime=2006-01"`
	}
	if err := Bind(c, &body); err != nil {
		return err
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
	if err := Bind(c, &in); err != nil {
		return err
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
	if err := Bind(c, &in); err != nil {
		return err
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
		// Targeting is per person, so the feed is filtered by who is asking
		// rather than by their role alone.
		ViewerID: UserID(c),
	})
	if err != nil {
		return err
	}
	if !isStaff {
		for i := range out {
			stripAnnounceTargeting(&out[i])
		}
	}
	return c.JSON(out)
}

// stripAnnounceTargeting removes the staff-only aim of an announcement before
// it goes to an ordinary reader. The target lists are the sensitive part: an
// announcement aimed via "ta_missing_documents" carries, in target_user_ids,
// the roster of exactly who has not turned their papers in — which is the
// kind of disclosure the private targeting exists to avoid.
func stripAnnounceTargeting(a *service.Announcement) {
	a.TargetUserIDs = nil
	a.TargetCourseIDs = nil
	a.TargetFilters = nil
	a.Recipients = nil
	a.AudienceCount = 0
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
		// Who was aimed at, who was emailed — staff bookkeeping, not part of
		// the notice. See stripAnnounceTargeting for why the target lists in
		// particular must never reach a reader.
		stripAnnounceTargeting(a)
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
	if err := Bind(c, &in); err != nil {
		return err
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
// AudiencePreview — POST /announcements/preview-audience — "who will get this".
//
// Answered by the same resolver that will actually deliver it, so the number
// on screen is the number of people reached, not an estimate.
func (h *AnnounceHandler) AudiencePreview(c *fiber.Ctx) error {
	var rule service.AudienceRule
	if err := Bind(c, &rule); err != nil {
		return err
	}
	out, err := h.Svc.Announce.PreviewAudience(c.Context(), rule)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// AudienceFilters — GET /announcements/audience-filters — the closed set of
// narrowing conditions, so the composer cannot offer one the resolver has no
// query for.
func (h *AnnounceHandler) AudienceFilters(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"items": service.AnnounceFilterOptions()})
}

// PublicGet — GET /public/announcements/:id — no authentication.
//
// The service decides what is visible; this handler adds nothing but the HTTP
// shape, so there is one place to reason about what an anonymous reader can
// reach. Anything not opted into sharing is reported as missing rather than
// forbidden: "this exists but you may not see it" is itself information.
func (h *AnnounceHandler) PublicGet(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "ไม่พบประกาศ")
	}
	a, err := h.Svc.Announce.PublicGet(c.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบประกาศ")
		}
		return err
	}
	return c.JSON(a)
}

// Resend — POST /announcements/:id/send-email — delivers to everyone on the
// audience who has not had it yet. Widening the target after publishing is the
// normal case; this is the button for it.
func (h *AnnounceHandler) Resend(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	sent, err := h.Svc.Announce.Deliver(c.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบประกาศ")
		}
		return err
	}
	return c.JSON(fiber.Map{"sent": sent})
}

// announceMediaKinds maps an accepted MIME to (kind, extension). The kind is
// decided HERE, from the sniffed bytes — never from the filename — because it
// is what the reader's browser will be told to do with the file.
var announceMediaKinds = map[string]struct {
	Kind string
	Ext  string
}{
	"image/jpeg":      {"image", ".jpg"},
	"image/jpg":       {"image", ".jpg"},
	"image/png":       {"image", ".png"},
	"image/webp":      {"image", ".webp"},
	"image/gif":       {"image", ".gif"},
	"video/mp4":       {"video", ".mp4"},
	"video/webm":      {"video", ".webm"},
	"video/quicktime": {"video", ".mov"},
	"application/pdf": {"file", ".pdf"},
}

// Per-kind ceilings. Video is the outlier: a two-minute clip from a phone is
// tens of megabytes, and refusing it would make the feature useless, while an
// unbounded upload would let one announcement fill the disk.
var announceMediaMaxBytes = map[string]int64{
	"image": 8 * 1024 * 1024,
	"video": 80 * 1024 * 1024,
	"file":  20 * 1024 * 1024,
}

// announceMediaMagic confirms the bytes really are what the MIME claims, for
// the formats where a wrong guess would be dangerous to serve.
func announceMediaMagic(head []byte, ext string) bool {
	switch ext {
	case ".pdf":
		return len(head) >= 5 && string(head[:5]) == "%PDF-"
	case ".mp4", ".mov":
		// ISO base media: bytes 4..8 are "ftyp".
		return len(head) >= 12 && string(head[4:8]) == "ftyp"
	case ".webm":
		return len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3
	case ".gif":
		return len(head) >= 6 && string(head[:3]) == "GIF"
	default:
		return imageMagicMatches(head, ext)
	}
}

// UploadMedia accepts one attachment — image, video or PDF — and returns the
// key plus the kind the composer should render it as.
func (h *AnnounceHandler) UploadMedia(c *fiber.Ctx) error {
	fh, err := c.FormFile("file")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "file required")
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(fh.Header.Get("Content-Type"), ";")[0]))
	spec, ok := announceMediaKinds[mime]
	if !ok {
		return fiber.NewError(fiber.StatusUnsupportedMediaType,
			"รองรับเฉพาะรูปภาพ (JPEG/PNG/WebP/GIF), วิดีโอ (MP4/WebM/MOV) และ PDF")
	}
	if max := announceMediaMaxBytes[spec.Kind]; fh.Size > max {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge,
			"ไฟล์ใหญ่เกิน "+strconv.FormatInt(max/(1024*1024), 10)+" MB")
	}
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	head := make([]byte, 16)
	n, _ := src.Read(head)
	if !announceMediaMagic(head[:n], spec.Ext) {
		return fiber.NewError(fiber.StatusUnsupportedMediaType, "ประเภทไฟล์ไม่ตรงกับเนื้อหา")
	}
	if _, err := src.Seek(0, 0); err != nil {
		return err
	}

	key, size, err := h.Svc.Storage.Save("announcements", uuid.New().String()+spec.Ext, src)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"kind": spec.Kind, "storage_key": key, "size_bytes": size, "mime": mime,
		"filename": fh.Filename,
		"url":      service.AttachmentURL(key),
	})
}

// ServeMedia streams an attachment to a signed-in reader.
func (h *AnnounceHandler) ServeMedia(c *fiber.Ctx) error {
	return serveAnnounceMedia(c, h.Svc, c.Params("*"), false)
}

// ServePublicMedia streams an attachment to anyone, but only when the
// announcement carrying it is publicly shared and live.
//
// Without this the public page renders <img> tags at an authenticated URL, so
// every shared announcement with a picture showed a broken image to exactly
// the people it was shared with.
func (h *AnnounceHandler) ServePublicMedia(c *fiber.Ctx) error {
	key := c.Params("*")
	ok, err := h.Svc.Announce.MediaIsPublic(c.Context(), key)
	if err != nil {
		return err
	}
	if !ok {
		// 404, not 403: an anonymous caller must not learn that a key exists.
		return fiber.NewError(fiber.StatusNotFound, "ไม่พบไฟล์")
	}
	return serveAnnounceMedia(c, h.Svc, key, true)
}

// serveAnnounceMedia is the shared streaming half — one place that decides
// content type and headers, so the public and private routes cannot drift.
func serveAnnounceMedia(c *fiber.Ctx, svc *service.Container, key string, shortCache bool) error {
	if key == "" {
		return fiber.NewError(fiber.StatusBadRequest, "key required")
	}
	if strings.Contains(key, "..") || !strings.HasPrefix(key, "announcements/") {
		return fiber.NewError(fiber.StatusBadRequest, "invalid key")
	}
	rc, err := svc.Storage.Open(key)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "ไม่พบไฟล์")
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	k := strings.TrimSuffix(key, ".enc")
	ct := "application/octet-stream"
	// Guard the slice: LastIndex on an extensionless key returns -1 and
	// k[-1:] panics. Our minted keys always carry an extension, but a served
	// path must not be one odd row away from a crash.
	ext := ""
	if dot := strings.LastIndex(k, "."); dot >= 0 {
		ext = strings.ToLower(k[dot:])
	}
	switch ext {
	case ".jpg", ".jpeg":
		ct = "image/jpeg"
	case ".png":
		ct = "image/png"
	case ".webp":
		ct = "image/webp"
	case ".gif":
		ct = "image/gif"
	case ".mp4":
		ct = "video/mp4"
	case ".webm":
		ct = "video/webm"
	case ".mov":
		ct = "video/quicktime"
	case ".pdf":
		ct = "application/pdf"
	}
	c.Set("Content-Type", ct)
	// Public files get a short window on purpose. Withdrawing an announcement
	// has to take its pictures down with it, and an hour-long cache would keep
	// serving them from the reader's own browser long after — verified on
	// 06/08/2026, when a withdrawn post's image still loaded from cache.
	if shortCache {
		c.Set("Cache-Control", "private, max-age=60, must-revalidate")
	} else {
		c.Set("Cache-Control", "private, max-age=3600")
	}
	// Stops a browser from re-interpreting a PDF or a clip as something it can
	// execute in the page's origin.
	c.Set("X-Content-Type-Options", "nosniff")
	return c.Send(body)
}

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
	out, err := h.Svc.Dashboard.Executive(c.Context(), termID, h.Svc.Budget, h.Svc.Appointment)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// Analytics — GET /dashboard/analytics — the executive budget view (monthly
// disbursement, per-curriculum, per-course). Read-only; also served to the
// synthetic executive role.
func (h *DashboardHandler) Analytics(c *fiber.Ctx) error {
	var termID *uuid.UUID
	if v := c.Query("term_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			termID = &id
		}
	}
	out, err := h.Svc.Dashboard.Analytics(c.Context(), termID, h.Svc.Budget, h.Svc.Export)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// AnalyticsXLSX — GET /dashboard/analytics.xlsx — the same figures as a
// three-sheet workbook for further analysis.
func (h *DashboardHandler) AnalyticsXLSX(c *fiber.Ctx) error {
	var termID *uuid.UUID
	if v := c.Query("term_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			termID = &id
		}
	}
	a, err := h.Svc.Dashboard.Analytics(c.Context(), termID, h.Svc.Budget, h.Svc.Export)
	if err != nil {
		return err
	}
	body, err := service.AnalyticsWorkbook(a)
	if err != nil {
		return err
	}
	name := "ta-budget-analytics.xlsx"
	if a.TermLabel != "" {
		name = "ta-budget-" + strings.ReplaceAll(a.TermLabel, "/", "-") + ".xlsx"
	}
	c.Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	c.Set("Content-Disposition", contentDisposition("attachment", name))
	return c.Send(body)
}

// TaOverview — GET /dashboard/ta/me — TA's per-course status + estimated pay.
func (h *DashboardHandler) TaOverview(c *fiber.Ctx) error {
	out, err := h.Svc.Dashboard.TaOverview(c.Context(), UserID(c), SelectedEnrollmentID(c))
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
	// HEAD must not reach the body below. Fiber's Get() registers HEAD on the
	// same handler, and this handler is not a read: it locks every fully
	// approved month, writes the PII access trail and records the batch. A
	// link prefetcher, a monitoring probe or a scanner issuing HEAD would
	// freeze a course's worklogs — irreversibly, since only an admin can
	// unlock — while transferring no file at all. Verified live on 04/08/2026:
	// a HEAD returned 200 and left a locked course and a history row behind.
	if c.Method() == fiber.MethodHead {
		return fiber.NewError(fiber.StatusMethodNotAllowed,
			"ต้องเรียกด้วย GET เท่านั้น เพราะการดาวน์โหลดมีผลล็อกข้อมูล")
	}
	raw := strings.TrimSuffix(c.Params("id"), ".zip")
	id, err := uuid.Parse(raw)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	// Resolve BEFORE anything uses it. An absent ?months= means "the whole
	// term", and passing that through as nil made the batch ledger record SQL
	// NULL — indistinguishable from a pre-split row, and invisible to the
	// `months && $1` round predicates, which left whole-term exports flagged as
	// still owing round 2 forever. See ExportService.ResolveCourseMonths.
	months, err := h.Svc.Export.ResolveCourseMonths(c.Context(), id, monthsParam(c))
	if err != nil {
		return err
	}
	body, name, taCount, err := h.Svc.Export.BuildCourseZip(c.Context(), id, months)
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
	if _, err := h.Svc.SubmissionPeriods.MarkCourseExported(c.Context(), actor, id, months); err != nil {
		return err
	}
	// Freeze section edits — this export is now the source of truth for the
	// course's section list + student counts. Non-fatal if it fails; the numbers
	// are already locked above and staff can mark sections manually if needed.
	_ = h.Svc.Teaching.MarkExported(c.Context(), id)
	// The export contains national IDs + bank details — audit who pulled it and
	// when (PII access trail, M5). This one must NOT be best-effort: the file is
	// about to leave the server carrying personal data, and an access trail with
	// a hole in it is the failure the trail exists to prevent. Refusing here
	// costs staff a retry; letting it through costs a PII disclosure nobody can
	// account for afterwards.
	if err := h.Svc.Auditor.Log(c.Context(), audit.Entry{ActorID: &actor, Action: "export.course", Entity: "teaching_course", EntityID: id.String(), IP: c.IP(), UserAgent: c.Get("User-Agent")}); err != nil {
		return err
	}

	// Keep the actual bytes so staff can re-download this exact file later
	// (BatchDownload) instead of only seeing a history row with no file behind
	// it. Best-effort like the Record below — a storage failure must not hide
	// the (already-locked) zip the request came here for; the history row is
	// simply left with an empty FilePath, which BatchDownload reports plainly.
	filePath := ""
	if key, _, serr := h.Svc.Storage.Save("export_batches", name, bytes.NewReader(body)); serr == nil {
		filePath = key
	} else {
		log.Printf("export_batch: failed to persist zip for re-download (course %s): %v", id, serr)
	}
	// Persist the batch so the dashboard can list history + budget snapshot.
	// TotalBaht is the ACTUAL paid sum (Σ actual_paid after the pro-rata cap) so
	// the recorded figure matches the ZIP the staff hands to finance — the old
	// Budget.UsedBaht used different math and never reconciled with the file.
	// Best-effort — a DB write failure must NOT hide the (already-locked) zip.
	if prev, perr := h.Svc.Export.CoursePreview(c.Context(), id, months); perr == nil {
		_, _ = h.Svc.ExportBatches.Record(c.Context(), actor, service.ExportBatch{
			TeachingCourseID: id,
			FilePath:         filePath,
			FileName:         name,
			TACount:          taCount,
			Months:           months,
			TotalBaht:        prev.TotalActual,
		})
	}

	c.Set("Content-Type", "application/zip")
	c.Set("Content-Disposition", contentDisposition("attachment", name))
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
	out, err := h.Svc.ExportBatches.DashboardSummary(c.Context(), h.Svc.Budget, h.Svc.Export, termID)
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
	out, err := h.Svc.Export.CoursePreview(c.Context(), id, monthsParam(c))
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// CourseExportCoverage — GET /exports/course/:id/coverage — the term's months
// with Thai labels, which of them this course has already exported, and where
// the budget year cuts. Drives the month picker on the payout screen.
func (h *ExportHandler) CourseExportCoverage(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.Export.CourseExportCoverage(c.Context(), id)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// BudgetSettlement reports which months of a course the budget can pay for.
// Readable by the course's lecturers and its TAs as well as staff — it decides
// their own pay, and warning them is the whole point of computing it early.
func (h *ExportHandler) BudgetSettlement(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("tcId"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.Export.SettlementForViewer(
		c.Context(), UserID(c), id, rbac.Has(Roles(c), rbac.RoleAdmin, rbac.RoleStaff))
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

// BatchDownload re-serves the exact ZIP a past export produced (see the
// export_batches history list), so staff don't have to re-run the export
// (which would re-lock already-locked months) just to get the file again.
func (h *ExportHandler) BatchDownload(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	b, err := h.Svc.ExportBatches.Get(c.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "ไม่พบประวัติการส่งออกนี้")
		}
		return err
	}
	if b.FilePath == "" {
		return fiber.NewError(fiber.StatusNotFound, "ไม่พบไฟล์ของการส่งออกนี้ในระบบจัดเก็บ (อาจเป็นรายการเก่าก่อนระบบเก็บไฟล์)")
	}
	rc, err := h.Svc.Storage.Open(b.FilePath)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound, "ไม่พบไฟล์ในระบบจัดเก็บ")
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	// The zip carries national IDs + bank details — audit re-downloads the
	// same as the original download (CourseZip). Not best-effort, same reason.
	actor := UserID(c)
	if err := h.Svc.Auditor.Log(c.Context(), audit.Entry{ActorID: &actor, Action: "export.batch_download", Entity: "export_batch", EntityID: b.ID.String(), IP: c.IP(), UserAgent: c.Get("User-Agent")}); err != nil {
		return err
	}
	c.Set("Content-Type", "application/zip")
	c.Set("Content-Disposition", contentDisposition("attachment", b.FileName))
	return c.Send(body)
}

// AppointmentPreview shows who the next คำสั่งแต่งตั้ง round would contain and
// which courses it skips, so staff confirm before a document that cannot be
// recalled gets printed.
func (h *ExportHandler) AppointmentPreview(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "term_id is required")
	}
	out, err := h.Svc.Appointment.Preview(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// AppointmentRounds lists the orders already issued for a term.
func (h *ExportHandler) AppointmentRounds(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Query("term_id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "term_id is required")
	}
	out, err := h.Svc.Appointment.ListRounds(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": out})
}

func (h *ExportHandler) AppointmentOrder(c *fiber.Ctx) error {
	var in service.AppointmentOrderInput
	if err := Bind(c, &in); err != nil {
		return err
	}
	body, name, err := h.Svc.Appointment.Build(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, docxContentType)
	c.Set(fiber.HeaderContentDisposition, contentDisposition("attachment", name))
	return c.Send(body)
}

// docxContentType is what a Word document is served as. Both appointment
// endpoints hand back a bare .docx since 06/08/2026 (the PDF, and the zip that
// wrapped the pair, were dropped) — announcing it as application/zip left the
// browser to guess from the extension alone.
const docxContentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// Certifier reports who will sign the ผู้รับรอง block on this term's claim
// forms — the explicit choice if one was made, otherwise the seat holder.
func (h *ExportHandler) Certifier(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid term id")
	}
	out, err := h.Svc.Export.ResolveCertifier(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// SetCertifier records the choice. An empty officer_id clears it, returning the
// term to following whoever holds the seat.
func (h *ExportHandler) SetCertifier(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid term id")
	}
	var body struct {
		// Empty clears the override (falls back to the seat holder), so this
		// is intentionally not `required` — only its shape is checked when set.
		OfficerID string `json:"officer_id" validate:"omitempty,uuid4"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
	var officer *uuid.UUID
	if strings.TrimSpace(body.OfficerID) != "" {
		id, err := uuid.Parse(body.OfficerID)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid officer id")
		}
		officer = &id
	}
	if err := h.Svc.Export.SetCertifier(c.Context(), UserID(c), termID, officer); err != nil {
		return err
	}
	out, err := h.Svc.Export.ResolveCertifier(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

// AppointmentReprint hands back a copy of an order already issued. It is a GET
// because it changes nothing: no new round, no ledger row, no renumbering —
// the same bytes the original download produced.
func (h *ExportHandler) AppointmentReprint(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	body, name, err := h.Svc.Appointment.Reprint(c.Context(), UserID(c), id)
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, docxContentType)
	c.Set(fiber.HeaderContentDisposition, contentDisposition("attachment", name))
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
		SELECT id, at, actor_id, actor_role::text, action, entity, entity_id, ip::text, before, after, note
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
		// before/after are JSONB, NULL for most actions (only writers that pass
		// audit.Entry.Before/After populate them). Scanned as raw bytes and
		// re-emitted as json.RawMessage so the API returns the actual object
		// (or null) instead of a stringified blob — every write already stores
		// this (audit.write, internal/audit/audit.go), this handler just never
		// read it back out.
		var before, after []byte
		if err := rows.Scan(&id, &at, &actorID, &actorRole, &action, &entity, &entityID, &ip, &before, &after, &note); err != nil {
			return err
		}
		out = append(out, fiber.Map{
			"id": id, "at": at, "actor_id": actorID, "actor_role": actorRole,
			"action": action, "entity": entity, "entity_id": entityID, "ip": ip,
			"before": json.RawMessage(before), "after": json.RawMessage(after), "note": note,
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
	if err := Bind(c, &in); err != nil {
		return err
	}
	out, err := h.Svc.AdminOfficers.Upsert(c.Context(), UserID(c), in)
	if err != nil {
		return err
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
	if err := Bind(c, &in); err != nil {
		return err
	}
	id, err := h.Svc.Holiday.Create(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": id})
}

func (h *HolidayHandler) BulkCreate(c *fiber.Ctx) error {
	// The wire body is a bare JSON array, not an {"items": [...]} envelope, so
	// this can't go through Bind: validator.Struct only accepts a struct (or
	// pointer to one) at the top level and errors out on a slice kind before
	// it ever looks at the `validate` tags on HolidayInput. Parse as before,
	// then validate each element with the same validator + error formatting
	// Bind uses internally, so a bad row in the batch gets the same kind of
	// message a single bad Create body would.
	var in []service.HolidayInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	for i := range in {
		if err := validate.Struct(&in[i]); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, humanizeValidationError(err))
		}
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
		NameTH string  `json:"name_th" validate:"required,max=200"`
		NameEN *string `json:"name_en,omitempty" validate:"omitempty,max=200"`
		Note   *string `json:"note,omitempty" validate:"omitempty,max=1000"`
		// Time window ("HH:MM"); both omitted/empty = all-day. Absent fields clear
		// the window, which is what the staff form sends when the user switches a
		// partial holiday back to "หยุดทั้งวัน". Format-only check here;
		// HolidayService.Patch (via normalizeHolidayWindow) still enforces "both
		// or neither" + end > start.
		StartTime *string `json:"start_time,omitempty" validate:"omitempty,datetime=15:04"`
		EndTime   *string `json:"end_time,omitempty" validate:"omitempty,datetime=15:04"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
	if err := h.Svc.Holiday.Patch(c.Context(), UserID(c), id, body.NameTH, body.NameEN, body.Note,
		body.StartTime, body.EndTime); err != nil {
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

// ============================================================================
// TDBMHandler — the webhook TDBM calls, plus a staff-facing manual trigger
// and sync history. See docs/TDBM-API-requirements.md and internal/service/tdbm.go.
// ============================================================================

type TDBMHandler struct{ Svc *service.Container }

// Webhook is POST /tdbm-webhook — called by TDBM, not a signed-in user (see
// VerifyTDBMWebhookSecret, the only gate in front of this route). The body is
// only ever {"event":"...","type":"holidays"|"extra-teachings"} and is purely
// informational: whatever it says, the response is the same full re-sync of
// the active term, so a malformed or empty body is not treated as an error —
// there is nothing in the payload this handler actually branches on.
//
// Answers immediately; the pull itself runs in the background via
// TriggerAsync so TDBM's request doesn't sit open for however long three
// upstream calls take.
func (h *TDBMHandler) Webhook(c *fiber.Ctx) error {
	var body struct {
		Event string `json:"event"`
		Type  string `json:"type"`
	}
	_ = c.BodyParser(&body)
	log.Printf("tdbm webhook received: event=%q type=%q", body.Event, body.Type)
	h.Svc.TDBM.TriggerAsync("webhook")
	return c.SendStatus(fiber.StatusOK)
}

// SyncNow is a staff-facing manual trigger (e.g. "ซิงก์ตอนนี้" button), for
// testing the pipeline or nudging it after fixing a config problem without
// waiting for the next webhook ping or hourly sweep. Runs synchronously —
// unlike the webhook path — so the caller's button can show the actual
// counts instead of "queued".
func (h *TDBMHandler) SyncNow(c *fiber.Ctx) error {
	results, err := h.Svc.TDBM.SyncAll(c.Context(), "manual")
	if err != nil {
		return err
	}
	return c.JSON(results)
}

// SyncLog lists recent tdbm_sync_log rows, newest first, so staff can see
// whether the last pull actually happened and what it found without reading
// server logs.
func (h *TDBMHandler) SyncLog(c *fiber.Ctx) error {
	limit, _ := strconv.Atoi(c.Query("limit", "50"))
	out, err := h.Svc.TDBM.RecentSyncLog(c.Context(), limit)
	if err != nil {
		return err
	}
	return c.JSON(out)
}
