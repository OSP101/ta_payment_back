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

func (h *TeachingHandler) UpdateCurriculum(c *fiber.Ctx) error {
	var in service.UpdateCurriculumInput
	if err := c.BodyParser(&in); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
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
		Curriculum string `json:"curriculum"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
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
		CourseIDs       []uuid.UUID `json:"course_ids"`
		PrimaryCourseID uuid.UUID   `json:"primary_course_id"`
		CurriculumCode  string      `json:"curriculum_code"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	groupID, err := h.Svc.Teaching.ConfirmCourseGroup(
		c.Context(), UserID(c), termID, body.PrimaryCourseID, body.CourseIDs, body.CurriculumCode)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"id": groupID})
}
