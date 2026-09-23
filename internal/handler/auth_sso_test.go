package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/auth"
	"ta-payment-back/internal/config"
	"ta-payment-back/internal/mail"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/storage"
	"ta-payment-back/internal/testutil"
)

// fakeSSONext answers /auth.token the way the manual says the real one does:
// HTTP 200 either way, ok:false for anything but the one live code, and the
// identity for it. Which email that identity carries is the test's choice.
func fakeSSONext(t *testing.T, email string) *httptest.Server {
	t.Helper()
	used := false
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/auth.token" || in["code"] != "live-code" || used {
			_, _ = w.Write([]byte(`{"ok":false,"error":"AUTH0001"}`))
			return
		}
		used = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "accessToken": "tok", "email": email, "citizenId": "1111111111111",
			"firstName": "KKU", "lastName": "Name",
		})
	}))
}

func newSSOApp(t *testing.T, apiBase string) (*fiber.App, *service.Container) {
	t.Helper()
	pool := testutil.NewPool(t)
	store, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		JWTSecret: "test-secret", JWTIssuer: "test", AppBaseURL: "http://localhost:3000",
		SSOEnabled: true, SSOAppID: "app", SSOClientID: "cid", SSOSecret: "sec",
		SSORedirect: "http://localhost:3000/login/sso", SSOAPIBase: apiBase, SSOLoginBase: apiBase,
	}
	svc := service.NewContainer(pool, store, mail.New(cfg), audit.New(pool), cfg, nil, nil)
	h := &AuthHandler{Svc: svc, Tokens: auth.NewTokenService(cfg.JWTSecret, cfg.JWTIssuer)}
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/auth/sso/url", h.SSOURL)
	app.Post("/auth/sso/exchange", h.SSOExchange)
	app.Post("/auth/sso/confirm", h.SSOConfirm)
	app.Post("/auth/logout", h.Logout)
	return app, svc
}

func postJSON(t *testing.T, app *fiber.App, path, body string, cookies ...*http.Cookie) (*http.Response, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res, out
}

func TestSSO_ExchangeThenConfirmMintsSession(t *testing.T) {
	kku := fakeSSONext(t, "Somchai.J@kkumail.com")
	defer kku.Close()
	app, svc := newSSOApp(t, kku.URL)

	userID := uuid.New()
	if _, err := svc.Pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1, 'somchai.j@kkumail.com', 'สมชาย', 'ใจดี', TRUE)`,
		userID); err != nil {
		t.Fatal(err)
	}

	// Step 1: code → ticket + who this is. No cookie yet.
	res, body := postJSON(t, app, "/auth/sso/exchange", `{"code":"live-code"}`)
	if res.StatusCode != 200 {
		t.Fatalf("exchange status = %d body = %v", res.StatusCode, body)
	}
	if body["email"] != "somchai.j@kkumail.com" || body["first_name"] != "สมชาย" {
		t.Errorf("confirm payload = %v", body)
	}
	if len(res.Cookies()) != 0 {
		t.Errorf("exchange must not set cookies, got %v", res.Cookies())
	}
	ticket, _ := body["ticket"].(string)
	if ticket == "" {
		t.Fatal("no ticket")
	}

	// Step 2: the click. Session + SSO marker cookie.
	res, body = postJSON(t, app, "/auth/sso/confirm", `{"ticket":"`+ticket+`"}`)
	if res.StatusCode != 200 {
		t.Fatalf("confirm status = %d body = %v", res.StatusCode, body)
	}
	var access, marker *http.Cookie
	for _, c := range res.Cookies() {
		switch c.Name {
		case "access_token":
			access = c
		case ssoSessionCookie:
			marker = c
		}
	}
	if access == nil || access.Value == "" {
		t.Fatal("no access_token cookie after confirm")
	}
	if marker == nil || marker.Value != "1" {
		t.Errorf("sso_session marker not set: %v", marker)
	}
	if u, _ := body["user"].(map[string]any); u == nil || u["email"] != "somchai.j@kkumail.com" {
		t.Errorf("confirm body = %v", body)
	}

	// A ticket is single use.
	if res, _ := postJSON(t, app, "/auth/sso/confirm", `{"ticket":"`+ticket+`"}`); res.StatusCode != 401 {
		t.Errorf("replayed ticket status = %d, want 401", res.StatusCode)
	}

	// Logout of an SSO session hands back the KKU logout URL.
	res, body = postJSON(t, app, "/auth/logout", `{}`, access, marker)
	if res.StatusCode != 200 || body["sso_logout_url"] != kku.URL+"/logout?app=app" {
		t.Errorf("logout = %d %v", res.StatusCode, body)
	}
	// ...but a password session (no marker) does not.
	if _, body := postJSON(t, app, "/auth/logout", `{}`, access); body["sso_logout_url"] != nil {
		t.Errorf("password logout leaked sso_logout_url: %v", body)
	}
}

func TestSSO_UnknownAccountIsRefusedWithTheEmail(t *testing.T) {
	kku := fakeSSONext(t, "nobody@kkumail.com")
	defer kku.Close()
	app, _ := newSSOApp(t, kku.URL)

	res, body := postJSON(t, app, "/auth/sso/exchange", `{"code":"live-code"}`)
	if res.StatusCode != 403 {
		t.Fatalf("status = %d body = %v", res.StatusCode, body)
	}
	if body["code"] != "sso_no_account" || body["kku_email"] != "nobody@kkumail.com" ||
		!strings.Contains(body["error"].(string), "nobody@kkumail.com") {
		t.Errorf("body = %v", body)
	}
	if _, has := body["ticket"]; has {
		t.Error("unknown account must not receive a ticket")
	}
}

func TestSSO_RejectedCodeAndDisabledService(t *testing.T) {
	kku := fakeSSONext(t, "x@kkumail.com")
	defer kku.Close()
	app, _ := newSSOApp(t, kku.URL)

	if res, _ := postJSON(t, app, "/auth/sso/exchange", `{"code":"stale"}`); res.StatusCode != 401 {
		t.Errorf("stale code status = %d, want 401", res.StatusCode)
	}
	if res, _ := postJSON(t, app, "/auth/sso/confirm", `{"ticket":"made-up"}`); res.StatusCode != 401 {
		t.Errorf("made-up ticket status = %d, want 401", res.StatusCode)
	}

	// Same wiring but with no SSO config: routes must go dark, not panic on
	// a nil client.
	off := func() *fiber.App {
		pool := testutil.NewPool(t)
		store, _ := storage.NewLocal(t.TempDir())
		cfg := config.Config{JWTSecret: "s", JWTIssuer: "t"}
		svc := service.NewContainer(pool, store, mail.New(cfg), audit.New(pool), cfg, nil, nil)
		h := &AuthHandler{Svc: svc, Tokens: auth.NewTokenService("s", "t")}
		a := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
		a.Get("/auth/sso/url", h.SSOURL)
		a.Post("/auth/sso/exchange", h.SSOExchange)
		return a
	}()
	req := httptest.NewRequest("GET", "/auth/sso/url", nil)
	res, _ := off.Test(req)
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if out["enabled"] != false {
		t.Errorf("disabled /auth/sso/url = %v", out)
	}
	if res, _ := postJSON(t, off, "/auth/sso/exchange", `{"code":"live-code"}`); res.StatusCode != 404 {
		t.Errorf("disabled exchange status = %d, want 404", res.StatusCode)
	}
}

// The login page needs KKU's logout URL for "use another account" — leaving
// the confirm card for /login alone keeps the KKU session alive.
func TestSSO_URLIncludesLogoutURL(t *testing.T) {
	app, _ := newSSOApp(t, "http://kku.test")
	res, err := app.Test(httptest.NewRequest("GET", "/auth/sso/url", nil))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	if out["enabled"] != true || out["logout_url"] != "http://kku.test/logout?app=app" {
		t.Fatalf("/auth/sso/url = %v", out)
	}
}
