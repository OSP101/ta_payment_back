package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/service"
)

func (h *TeachingHandler) ListCurricula(c *fiber.Ctx) error {
	out, err := h.Svc.Teaching.ListCurricula(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *TeachingHandler) CreateCurriculum(c *fiber.Ctx) error {
	var in service.CreateCurriculumInput
	if err := Bind(c, &in); err != nil {
		return err
	}
	out, err := h.Svc.Teaching.CreateCurriculum(c.Context(), UserID(c), in)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(out)
}
func (h *TeachingHandler) DeleteCurriculum(c *fiber.Ctx) error {
	if err := h.Svc.Teaching.DeleteCurriculum(c.Context(), UserID(c), c.Params("code")); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *TeachingHandler) UpdateCurriculum(c *fiber.Ctx) error {
	var in service.UpdateCurriculumInput
	if err := Bind(c, &in); err != nil {
		return err
	}
	if err := h.Svc.Teaching.UpdateCurriculum(c.Context(), UserID(c), c.Params("code"), in); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *TeachingHandler) UpdateSectionCurriculum(c *fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var body struct {
		// The service validates against the current curricula table.
		Curriculum string `json:"curriculum" validate:"required,max=40"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
	if err := h.Svc.Teaching.UpdateSectionCurriculum(c.Context(), UserID(c), id, body.Curriculum); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *TeachingHandler) CourseGroupCandidates(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	out, err := h.Svc.Teaching.DetectCourseGroups(c.Context(), termID)
	if err != nil {
		return err
	}
	return c.JSON(out)
}

func (h *TeachingHandler) ConfirmCourseGroup(c *fiber.Ctx) error {
	termID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid id")
	}
	var body struct {
		CourseIDs       []uuid.UUID `json:"course_ids" validate:"required,min=2"`
		PrimaryCourseID uuid.UUID   `json:"primary_course_id" validate:"required"`
		CurriculumCode  string      `json:"curriculum_code" validate:"required"`
	}
	if err := Bind(c, &body); err != nil {
		return err
	}
	groupID, err := h.Svc.Teaching.ConfirmCourseGroup(
		c.Context(), UserID(c), termID, body.PrimaryCourseID, body.CourseIDs, body.CurriculumCode)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"id": groupID})
}
