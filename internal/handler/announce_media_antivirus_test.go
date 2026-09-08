package handler

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/antivirus"
	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

type fakeAnnounceScanner struct {
	err     error
	enabled bool
	calls   int
}

func (f *fakeAnnounceScanner) Scan(context.Context, io.Reader) error { f.calls++; return f.err }
func (f *fakeAnnounceScanner) Enabled() bool                         { return f.enabled }

// PDPA-02: POST /announcements/upload-media was the only one of four upload
// paths that never ran through a virus scanner, despite being the most
// exposed — the file it stores is served to anonymous readers via
// GET /public/announcements/media/*. This pins that ScanUpload is now
// actually consulted, both when it flags a file and when the scanner is off.
func TestUploadMedia_RunsThroughAntivirus(t *testing.T) {
	pool := testutil.NewPool(t)
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.New(pool)
	svc := service.NewContainer(pool, store, mail.New(config.Config{}), aud, config.Config{}, nil, nil)

	actor := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name) VALUES ($1, $2, 'ท', 'ท')`,
		actor, actor.String()+"@test.local"); err != nil {
		t.Fatal(err)
	}

	ah := &AnnounceHandler{Svc: svc}
	buildApp := func() *fiber.App {
		app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
		app.Post("/announcements/upload-media", func(c *fiber.Ctx) error {
			c.Locals(string(CtxUserID), actor)
			return c.Next()
		}, ah.UploadMedia)
		return app
	}

	// EICAR body, header forged to pass the PDF magic-byte sniff — exactly the
	// shape the plan's own attack scenario describes: a file that looks like a
	// PDF to the sniff check but is not one.
	eicar := append([]byte("%PDF-"), []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)...)

	post := func(app *fiber.App) int {
		t.Helper()
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		part, err := w.CreatePart(map[string][]string{
			"Content-Disposition": {`form-data; name="file"; filename="fake.pdf"`},
			"Content-Type":        {"application/pdf"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(eicar); err != nil {
			t.Fatal(err)
		}
		w.Close()
		req := httptest.NewRequest("POST", "/announcements/upload-media", &body)
		req.Header.Set("Content-Type", w.FormDataContentType())
		res, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	// Scanner flags it: must be refused, and must never reach the store.
	svc.AV = &fakeAnnounceScanner{enabled: true, err: &antivirus.ErrInfected{Signature: "Eicar-Test-Signature"}}
	if got := post(buildApp()); got != fiber.StatusUnprocessableEntity {
		t.Errorf("infected upload: status = %d, want 422", got)
	}

	// Scanner disabled (as it is by default without CLAMAV_ADDR): must pass —
	// this is the existing fail-open-when-unconfigured behaviour, unchanged.
	svc.AV = &fakeAnnounceScanner{enabled: false}
	if got := post(buildApp()); got != fiber.StatusCreated {
		t.Errorf("scanner disabled: status = %d, want 201", got)
	}
}
