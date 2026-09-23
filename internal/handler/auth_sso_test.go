package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	return newSSOAppWith(t, apiBase, false)
}

func newSSOAppWith(t *testing.T, apiBase string, singleLogout bool) (*fiber.App, *service.Container) {
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
		SSOSingleLogout: singleLogout,
	}
	svc := service.NewContainer(pool, store, mail.New(cfg), audit.New(pool), cfg, nil, nil)
	h := &AuthHandler{Svc: svc, Tokens: auth.NewTokenService(cfg.JWTSecret, cfg.JWTIssuer)}
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Get("/auth/sso/url", h.SSOURL)
	app.Get("/auth/sso/login", h.SSOLoginStart)
	app.Post("/auth/sso/exchange", h.SSOExchange)
	app.Post("/auth/sso/confirm", h.SSOConfirm)
	app.Post("/auth/logout", h.Logout)
	return app, svc
}

// ssoNonce is what GET /auth/sso/login plants before the browser leaves for
// KKU; every exchange must carry one, so the tests mint one the same way.
func ssoNonce(t *testing.T) *http.Cookie {
	t.Helper()
	v, err := newSSONonce("test-secret", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: ssoNonceCookie, Value: v}
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
	res, body := postJSON(t, app, "/auth/sso/exchange", `{"code":"live-code"}`, ssoNonce(t))
	if res.StatusCode != 200 {
		t.Fatalf("exchange status = %d body = %v", res.StatusCode, body)
	}
	if body["email"] != "somchai.j@kkumail.com" || body["first_name"] != "สมชาย" {
		t.Errorf("confirm payload = %v", body)
	}
	// It may clear the consumed nonce, but it must hand out nothing that
	// stands in for a session — that is the confirm click's job.
	for _, c := range res.Cookies() {
		if c.Name != ssoNonceCookie || c.Value != "" {
			t.Errorf("exchange set %s=%q; only a cleared nonce is allowed", c.Name, c.Value)
		}
	}
	ticket, _ := body["ticket"].(string)
	if ticket == "" {
		t.Fatal("no ticket")
	}

	// Step 2: the click. A session, and nothing that marks it as an SSO one
	// — logging out is local-only, so there is nothing left to remember.
	res, body = postJSON(t, app, "/auth/sso/confirm", `{"ticket":"`+ticket+`"}`)
	if res.StatusCode != 200 {
		t.Fatalf("confirm status = %d body = %v", res.StatusCode, body)
	}
	var access *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "access_token" {
			access = c
		}
		if strings.HasPrefix(c.Name, "sso_") {
			t.Errorf("confirm set a leftover SSO cookie: %s", c.Name)
		}
	}
	if access == nil || access.Value == "" {
		t.Fatal("no access_token cookie after confirm")
	}
	if u, _ := body["user"].(map[string]any); u == nil || u["email"] != "somchai.j@kkumail.com" {
		t.Errorf("confirm body = %v", body)
	}

	// A ticket is single use.
	if res, _ := postJSON(t, app, "/auth/sso/confirm", `{"ticket":"`+ticket+`"}`); res.StatusCode != 401 {
		t.Errorf("replayed ticket status = %d, want 401", res.StatusCode)
	}

	// Logging out of an SSO session is local-only: our cookie is cleared and
	// that is all. Sending the browser to KKU's /logout would end the KKU
	// session itself, signing the person out of every other KKU system for
	// the sake of leaving this one.
	res, body = postJSON(t, app, "/auth/logout", `{}`, access)
	if res.StatusCode != 200 || body["ok"] != true {
		t.Fatalf("logout = %d %v", res.StatusCode, body)
	}
	if body["sso_logout_url"] != nil {
		t.Errorf("logout must not send the browser to KKU: %v", body)
	}
	var cleared *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "access_token" {
			cleared = c
		}
	}
	if cleared == nil || cleared.Value != "" {
		t.Errorf("logout did not clear access_token: %v", cleared)
	}
}

func TestSSO_UnknownAccountIsRefusedWithTheEmail(t *testing.T) {
	kku := fakeSSONext(t, "nobody@kkumail.com")
	defer kku.Close()
	app, _ := newSSOApp(t, kku.URL)

	res, body := postJSON(t, app, "/auth/sso/exchange", `{"code":"live-code"}`, ssoNonce(t))
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

	if res, _ := postJSON(t, app, "/auth/sso/exchange", `{"code":"stale"}`, ssoNonce(t)); res.StatusCode != 401 {
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
	if res, _ := postJSON(t, off, "/auth/sso/exchange", `{"code":"live-code"}`, ssoNonce(t)); res.StatusCode != 404 {
		t.Errorf("disabled exchange status = %d, want 404", res.StatusCode)
	}
}

// KKU_SSO_SINGLE_LOGOUT=true is the opt-in for the other trade-off: a
// deployment on shared lab machines wants leaving this app to end the KKU
// session as well, so the next person sitting down does not inherit it.
func TestSSO_SingleLogoutOptIn(t *testing.T) {
	kku := fakeSSONext(t, "somchai.j@kkumail.com")
	defer kku.Close()
	app, svc := newSSOAppWith(t, kku.URL, true)

	if _, err := svc.Pool.Exec(context.Background(),
		`INSERT INTO users (id, email, first_name, last_name, is_active) VALUES ($1, 'somchai.j@kkumail.com', 'ส', 'ใ', TRUE)`,
		uuid.New()); err != nil {
		t.Fatal(err)
	}
	_, body := postJSON(t, app, "/auth/sso/exchange", `{"code":"live-code"}`, ssoNonce(t))
	ticket, _ := body["ticket"].(string)
	if ticket == "" {
		t.Fatalf("no ticket: %v", body)
	}
	res, _ := postJSON(t, app, "/auth/sso/confirm", `{"ticket":"`+ticket+`"}`)
	var access, marker *http.Cookie
	for _, c := range res.Cookies() {
		switch c.Name {
		case "access_token":
			access = c
		case ssoSessionCookie:
			marker = c
		}
	}
	if marker == nil || marker.Value != "1" {
		t.Fatalf("single logout on: SSO session not marked: %v", marker)
	}
	_, body = postJSON(t, app, "/auth/logout", `{}`, access, marker)
	if body["sso_logout_url"] != kku.URL+"/logout?app=app" {
		t.Errorf("logout = %v, want the KKU logout URL", body)
	}
	// Even with the flag on, a password-only session is nobody's KKU session
	// to end — no marker, no hop.
	if _, body := postJSON(t, app, "/auth/logout", `{}`, access); body["sso_logout_url"] != nil {
		t.Errorf("password session sent to KKU logout: %v", body)
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
	if out["enabled"] != true || out["logout_url"] != "http://kku.test/logout?app=app" ||
		out["url"] != "/api/v1/auth/sso/login" {
		t.Fatalf("/auth/sso/url = %v", out)
	}
}

// The nonce is the stand-in for SSONext's missing `state` (auth_sso_nonce.go):
// a code that did not start its life in THIS browser never reaches KKU.
func TestSSO_ExchangeRequiresTheLoginNonce(t *testing.T) {
	kku := fakeSSONext(t, "somchai.j@kkumail.com")
	defer kku.Close()
	app, _ := newSSOApp(t, kku.URL)

	// The start hop plants it and sends the browser on to KKU.
	res, err := app.Test(httptest.NewRequest("GET", "/auth/sso/login", nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 302 || res.Header.Get("Location") != kku.URL+"/login?app=app" {
		t.Fatalf("start = %d → %q", res.StatusCode, res.Header.Get("Location"))
	}
	var planted *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == ssoNonceCookie {
			planted = c
		}
	}
	if planted == nil || planted.Value == "" {
		t.Fatal("start hop planted no nonce")
	}

	// A mailed callback link — no nonce at all.
	if res, _ := postJSON(t, app, "/auth/sso/exchange", `{"code":"live-code"}`); res.StatusCode != 401 {
		t.Errorf("exchange without a nonce = %d, want 401", res.StatusCode)
	}
	// A forged one: right shape, wrong signature.
	forged := &http.Cookie{Name: ssoNonceCookie, Value: "YWJj.bm90LWEtc2ln"}
	if res, _ := postJSON(t, app, "/auth/sso/exchange", `{"code":"live-code"}`, forged); res.StatusCode != 401 {
		t.Errorf("forged nonce = %d, want 401", res.StatusCode)
	}
	// Expired, signed correctly.
	old, err := newSSONonce("test-secret", time.Now().Add(-2*ssoNonceTTL))
	if err != nil {
		t.Fatal(err)
	}
	stale := &http.Cookie{Name: ssoNonceCookie, Value: old}
	if res, _ := postJSON(t, app, "/auth/sso/exchange", `{"code":"live-code"}`, stale); res.StatusCode != 401 {
		t.Errorf("expired nonce = %d, want 401", res.StatusCode)
	}

	// The real one works — and is consumed, so the same code cannot be run
	// through this browser twice.
	live := &http.Cookie{Name: ssoNonceCookie, Value: planted.Value}
	res2, body := postJSON(t, app, "/auth/sso/exchange", `{"code":"live-code"}`, live)
	if res2.StatusCode != 403 {
		// No such account in this test's DB — but it got past the nonce and
		// all the way to KKU, which is what is being pinned here.
		t.Fatalf("live nonce = %d %v, want the exchange to proceed", res2.StatusCode, body)
	}
	var cleared *http.Cookie
	for _, c := range res2.Cookies() {
		if c.Name == ssoNonceCookie {
			cleared = c
		}
	}
	if cleared == nil || cleared.Value != "" {
		t.Errorf("nonce not consumed: %v", cleared)
	}
}
