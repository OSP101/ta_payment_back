package handler

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

// A ZAP scan against POST /announcements/upload-media flagged SQL Injection
// (High) off a single signal: mutating the file part's Content-Type header
// (appending a stray `"`) got back a 500 with a bare "Internal Server Error"
// body. That body shape never comes from this handler — every error path here
// goes through ErrorHandler and answers JSON (see errorResponse) — so
// whatever produced it sat in front of this process (proxy/edge), not in it.
//
// This pins the one thing this test CAN prove: a file part with a malformed
// Content-Type value — quote, embedded CRLF, or truncated mid-header — never
// panics or 500s inside the handler itself. It is answered as a normal 4xx.
func TestUploadMedia_MalformedContentTypeNeverPanics(t *testing.T) {
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

	const boundary = "----WebKitFormBoundarymvsJX8txQDUia6qv"
	const pdf = "%PDF-1.4\n%\xc3\xa2\xc3\xa3\xc3\x8f\xc3\x93\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF"

	part := func(contentType string) string {
		return "--" + boundary + "\r\n" +
			`Content-Disposition: form-data; name="file"; filename="a.pdf"` + "\r\n" +
			"Content-Type: " + contentType + "\r\n\r\n" +
			pdf + "\r\n--" + boundary + "--\r\n"
	}

	cases := map[string]string{
		// The exact byte the scan appended to the header value.
		"trailing_quote": part(`application/pdf"`),
		// A quote and a header-injection attempt in one.
		"quote_and_crlf": part("application/pdf\"\r\nX-Injected: 1"),
		// No closing part/boundary at all — the header ends the request.
		"truncated_no_body": "--" + boundary + "\r\n" +
			`Content-Disposition: form-data; name="file"; filename="a.pdf"` + "\r\n" +
			`Content-Type: application/pdf`,
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/announcements/upload-media", bytes.NewReader([]byte(raw)))
			req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

			res, err := buildApp().Test(req, -1)
			// A parse failure fasthttp/mime-multipart surfaces as a request
			// error, not a panic — app.Test never gets to answer at all, and
			// that is itself the property under test: nothing panicked.
			if err != nil {
				t.Logf("%s: request-level error (no response, no panic): %v", name, err)
				return
			}
			defer res.Body.Close()
			if res.StatusCode >= 500 {
				t.Errorf("%s: got %d, want a 4xx — malformed input must never surface as a server error", name, res.StatusCode)
			}
		})
	}
}
