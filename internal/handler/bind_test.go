package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestBind(t *testing.T) {
	type req struct {
		Email string `json:"email" validate:"required,email"`
		Name  string `json:"name" validate:"required,min=2,max=50"`
	}

	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Post("/probe", func(c *fiber.Ctx) error {
		var in req
		if err := Bind(c, &in); err != nil {
			return err
		}
		return c.JSON(in)
	})

	post := func(body string) (int, string) {
		t.Helper()
		httpReq := httptest.NewRequest("POST", "/probe", strings.NewReader(body))
		httpReq.Header.Set("Content-Type", "application/json")
		res, err := app.Test(httpReq)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		buf := make([]byte, 1024)
		n, _ := res.Body.Read(buf)
		return res.StatusCode, string(buf[:n])
	}

	if status, body := post(`{"email":"a@b.com","name":"Ann"}`); status != 200 {
		t.Errorf("valid body should pass, got %d: %s", status, body)
	}
	if status, body := post(`{"name":"Ann"}`); status != 400 || !strings.Contains(body, "กรุณาระบุอีเมล") {
		t.Errorf("missing required email should 400 and name the field, got %d: %s", status, body)
	}
	if status, body := post(`{"email":"not-an-email","name":"Ann"}`); status != 400 || !strings.Contains(body, "อีเมล") {
		t.Errorf("invalid email should 400 and name the field, got %d: %s", status, body)
	}
	if status, body := post(`{"email":"a@b.com","name":"A"}`); status != 400 || !strings.Contains(body, "ชื่อ ต้องมีอย่างน้อย 2 ตัวอักษร") {
		t.Errorf("too-short name should 400 and name the field, got %d: %s", status, body)
	}
	if status, _ := post(`not json`); status != 400 {
		t.Errorf("malformed JSON should still 400, got %d", status)
	}
}

// Validator failures used to come back in English ("num_students must be 0 or
// greater"), which the frontend refuses to show (it only shows Thai server
// text) — the user saw "ทำรายการไม่สำเร็จ (รหัส 400)" with no reason. Each
// common tag must now produce a Thai sentence naming the field.
func TestHumanizeValidationErrorThai(t *testing.T) {
	type req struct {
		NumStudents int      `json:"num_students" validate:"gte=0"`
		LecturerIDs []string `json:"lecturer_ids" validate:"min=1"`
		Note        string   `json:"note" validate:"max=5"`
		Level       string   `json:"study_level" validate:"omitempty,oneof=bachelor master"`
		Mystery     int      `json:"mystery_field" validate:"lte=3"`
	}
	cases := []struct {
		in   req
		want string
	}{
		{req{NumStudents: -1, LecturerIDs: []string{"a"}}, "จำนวนนักศึกษา ต้องไม่น้อยกว่า 0"},
		{req{LecturerIDs: nil}, "อาจารย์ผู้สอน ต้องเลือกอย่างน้อย 1 รายการ"},
		{req{LecturerIDs: []string{"a"}, Note: "123456"}, "หมายเหตุ ยาวได้ไม่เกิน 5 ตัวอักษร"},
		{req{LecturerIDs: []string{"a"}, Level: "x"}, "ระดับการศึกษา ต้องเป็นค่าใดค่าหนึ่งต่อไปนี้: bachelor, master"},
		// Unknown field: falls back to the JSON key, still inside a Thai sentence.
		{req{LecturerIDs: []string{"a"}, Mystery: 9}, "mystery_field ต้องไม่เกิน 3"},
	}
	for _, c := range cases {
		got := humanizeValidationError(validate.Struct(&c.in))
		if got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}
