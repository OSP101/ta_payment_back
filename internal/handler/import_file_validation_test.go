package handler

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

// Found during the 2026-09-14 TOR acceptance review (§3.3 ข.3-4): ImportExcel
// read the uploaded body and handed it straight to excelize with no check on
// extension or content, so a wrong-format file surfaced excelize's own raw
// English error ("zip: not a valid zip file") instead of anything an officer
// could act on. These pin the fix: reject before parsing, in Thai.
func newImportApp(t *testing.T, actor uuid.UUID) *fiber.App {
	t.Helper()
	pool := testutil.NewPool(t)
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), audit.New(pool), config.Config{}, nil, nil)

	// BodyLimit matches main.go's real setting — the default (4 MB) would
	// reject a 21 MB request at the framework level before it ever reaches
	// ImportExcel's own 20 MB check, which is what TestImportExcel_
	// RejectsOversizedFile means to exercise.
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler, BodyLimit: 32 * 1024 * 1024})
	th := &TeachingHandler{Svc: svc}
	app.Post("/teaching-courses/import", func(c *fiber.Ctx) error {
		c.Locals(string(CtxUserID), actor)
		return c.Next()
	}, th.ImportExcel)
	return app
}

func postImport(t *testing.T, app *fiber.App, filename string, body []byte, dryRun bool) int {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("term_id", uuid.New().String()); err != nil {
		t.Fatal(err)
	}
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	w.Close()

	url := "/teaching-courses/import"
	if dryRun {
		url += "?dry_run=1"
	}
	req := httptest.NewRequest("POST", url, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestImportExcel_RejectsWrongExtension(t *testing.T) {
	app := newImportApp(t, uuid.New())
	status := postImport(t, app, "รายวิชา.csv", []byte("code,name\nCP101,Test\n"), true)
	if status != fiber.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (UnprocessableEntity)", status, fiber.StatusUnprocessableEntity)
	}
}

func TestImportExcel_RejectsXlsxNamedFileThatIsNotAZip(t *testing.T) {
	app := newImportApp(t, uuid.New())
	// Right extension, wrong content — the exact case that used to reach
	// excelize and surface "zip: not a valid zip file".
	status := postImport(t, app, "รายวิชาที่เปิดสอน.xlsx", []byte("this is not a zip file at all"), true)
	if status != fiber.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d (UnprocessableEntity)", status, fiber.StatusUnprocessableEntity)
	}
}

func TestImportExcel_RejectsOversizedFile(t *testing.T) {
	app := newImportApp(t, uuid.New())
	big := make([]byte, 21<<20) // 21 MB, over the 20 MB cap
	copy(big, "PK\x03\x04")
	status := postImport(t, app, "big.xlsx", big, true)
	if status != fiber.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d (RequestEntityTooLarge)", status, fiber.StatusRequestEntityTooLarge)
	}
}

// A file that passes the extension+magic-bytes gate must still reach the real
// import logic (and get the actual authorization/business-logic answer, not a
// generic file-format rejection).
func TestImportExcel_ValidXlsxPassesFileTypeGate(t *testing.T) {
	pool := testutil.NewPool(t)
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), audit.New(pool), config.Config{}, nil, nil)

	admin := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1, $2, 'ผู้ดูแล', 'ทดสอบ', TRUE)`,
		admin, admin.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_roles (user_id, role) VALUES ($1, 'admin'::role_code)`, admin); err != nil {
		t.Fatal(err)
	}

	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	th := &TeachingHandler{Svc: svc}
	app.Post("/teaching-courses/import", func(c *fiber.Ctx) error {
		c.Locals(string(CtxUserID), admin)
		return c.Next()
	}, th.ImportExcel)

	xlsx := buildMinimalNormalizedXlsx(t)
	status := postImport(t, app, "รายวิชาที่เปิดสอน.xlsx", xlsx, true)
	if status == fiber.StatusUnprocessableEntity || status == fiber.StatusRequestEntityTooLarge {
		t.Fatalf("a valid .xlsx was rejected by the file-type gate: status %d", status)
	}
	if status != fiber.StatusOK {
		t.Errorf("status = %d, want 200 (preview of a well-formed file)", status)
	}
}

// buildMinimalNormalizedXlsx makes the smallest file parseNormalizedSheet
// accepts — a "Normalized" sheet with just the header row. No data rows is
// fine: the parser returns an empty course list, not an error.
func buildMinimalNormalizedXlsx(t *testing.T) []byte {
	t.Helper()
	f := excelize.NewFile()
	if err := f.SetSheetName("Sheet1", "Normalized"); err != nil {
		t.Fatal(err)
	}
	if err := f.SetSheetRow("Normalized", "A1", &[]string{
		"CourseCode", "CourseName", "Unit", "Section",
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
