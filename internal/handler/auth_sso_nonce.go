package handler

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// SSONext has no `state` parameter (see the manual and service.SSOService),
// so a callback URL with a code in it is a bearer credential anyone can mail
// to anyone. This is the missing half of that parameter, done the same way
// the sibling KKU app does it (OSP101/itii-assist-classroom-back's
// kku_sso_handler.go): GET /auth/sso/login plants a signed, short-lived,
// single-use nonce cookie and only then sends the browser to KKU, and the
// callback's exchange refuses a code that does not arrive with a live one.
//
// It is signed with JWTSecret so a forged cookie cannot be minted client
// side. Nothing in it identifies a user — the value is random and the
// payload is just an expiry — so it is not a credential, only proof that
// THIS browser is the one that started THIS login.
//
// The confirm card is still the primary defence, and outranks this: a
// browser that did start a login can still be handed someone else's code,
// and only a human reading "เข้าสู่ระบบด้วยบัญชี x@kkumail.com" catches
// that. The nonce just means such a code never reaches KKU's /auth.token.
const (
	ssoNonceCookie = "sso_nonce"
	ssoNonceTTL    = 10 * time.Minute
)

// newSSONonce returns the cookie value: base64(expiry) "." base64(HMAC).
func newSSONonce(secret string, now time.Time) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw) + ":" +
		strconv.FormatInt(now.Add(ssoNonceTTL).Unix(), 10)
	b64 := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return b64 + "." + signSSONonce(secret, b64), nil
}

func signSSONonce(secret, payloadB64 string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payloadB64))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validSSONonce reports whether value carries our signature and is unexpired.
func validSSONonce(secret, value string, now time.Time) bool {
	payloadB64, sig, ok := strings.Cut(value, ".")
	if !ok || payloadB64 == "" {
		return false
	}
	// Constant time: a byte-by-byte comparison here would leak the expected
	// signature one character at a time to a caller willing to retry.
	if !hmac.Equal([]byte(sig), []byte(signSSONonce(secret, payloadB64))) {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return false
	}
	_, expStr, ok := strings.Cut(string(payload), ":")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	return err == nil && now.Unix() < exp
}

func setSSONonceCookie(c *fiber.Ctx, value string, secure bool) {
	exp := time.Now().Add(ssoNonceTTL)
	if value == "" {
		exp = time.Now().Add(-time.Hour)
	}
	c.Cookie(&fiber.Cookie{
		Name: ssoNonceCookie, Value: value, HTTPOnly: true,
		// Lax, not Strict: KKU redirects the browser BACK to our callback,
		// and a Strict cookie is withheld on a cross-site navigation — the
		// nonce would never be readable at the moment it is needed.
		SameSite: "Lax", Secure: secure, Path: "/", Expires: exp,
	})
}

// SSOLoginStart is GET /auth/sso/login — where the "เข้าสู่ระบบด้วย KKU"
// button points. It exists so the nonce above can be planted before the
// browser leaves for KKU; it then 302s straight on, so the person sees one
// hop, not a page.
func (h *AuthHandler) SSOLoginStart(c *fiber.Ctx) error {
	if !h.Svc.SSO.Enabled() {
		return fiber.NewError(fiber.StatusNotFound, "SSO is not enabled")
	}
	nonce, err := newSSONonce(h.Svc.Cfg.JWTSecret, time.Now())
	if err != nil {
		return err
	}
	setSSONonceCookie(c, nonce, h.Svc.Cfg.CookieSecure)
	c.Set("Cache-Control", "no-store")
	return c.Redirect(h.Svc.SSO.LoginURL(), fiber.StatusFound)
}
