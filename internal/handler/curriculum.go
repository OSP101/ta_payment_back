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
		// Mirrors validSectionCurricula (service/curriculum.go) — the CHECK
		// constraint's actual value set, kept here as a first-line 400 instead
		// of a round trip to the service's own Invalid() check.
		Curriculum string `json:"curriculum" validate:"required,oneof=CS IT GIS AI CY OTHER KKBS"`
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
