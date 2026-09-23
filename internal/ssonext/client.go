// Package ssonext talks to KKU Single Sign On (SSONext), the identity
// service สำนักเทคโนโลยีดิจิทัล runs for university web apps. It is NOT
// OAuth 2.0 — the shapes here follow "คู่มือการใช้งาน KKU Single Sign On
// (SSONext)" exactly:
//
//	login   GET  {LoginBase}/login?app=<AppID>          → redirects to RedirectURL?code=<uuid>
//	token   POST {APIBase}/auth.token   (JSON body)     → identity + accessToken
//	profile POST {APIBase}/user.profile (Bearer token)  → extended profile
//	logout  GET  {LoginBase}/logout?app=<AppID>         → redirects to the registered logout URL
//
// Two things the manual makes clear that differ from a standard provider and
// that every caller must keep in mind:
//
//   - The redirect URL is registered with KKU up front and is NOT a request
//     parameter of the login step. The same string is echoed back to
//     /auth.token as redirectUrl, so config.SSORedirect must match the
//     registered value byte for byte.
//   - Failures from /auth.token come back as HTTP 200 with {"ok": false,
//     "error": "AUTH0001"}; only /user.profile uses a real 401. Exchange
//     therefore checks ok, not the status code.
//
// The service also returns citizenId (เลขบัตรประชาชน) and, on the profile
// call, phone number and gender. This package deliberately has no fields
// for those: the decoder drops them on the floor, so they can never be
// logged, audited, or persisted by accident. TA citizen IDs already have
// their own encrypted, purpose-limited home (ta_profiles.citizen_id_enc) —
// a login should not become a second copy.
package ssonext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Default production endpoints from the manual. Overridable so a dev machine
// can point at cmd/ssonext-mock and tests at an httptest server.
const (
	DefaultLoginBase = "https://ssonext.kku.ac.th"
	DefaultAPIBase   = "https://ssonext-api.kku.ac.th"
)

// maxBody caps what we are willing to read from the identity service — the
// real responses are a few hundred bytes.
const maxBody = 64 << 10

var (
	// ErrRejected is /auth.token answering ok:false — a code that is unknown,
	// expired, already used, or a clientId/secret/redirectUrl mismatch. The
	// service's own error code is carried in RejectedError.Code for logs; the
	// user-facing handler shows one generic message regardless.
	ErrRejected = errors.New("ssonext: code rejected")
	// ErrUnauthorized is /user.profile answering 401 — the access token is no
	// longer good.
	ErrUnauthorized = errors.New("ssonext: access token rejected")
)

// RejectedError wraps ErrRejected with the service's error code.
type RejectedError struct{ Code string }

func (e *RejectedError) Error() string { return "ssonext: code rejected (" + e.Code + ")" }
func (e *RejectedError) Unwrap() error { return ErrRejected }

// Client holds the per-app credentials. Zero-value fields for LoginBase /
// APIBase / HTTP fall back to the production defaults.
type Client struct {
	LoginBase    string
	APIBase      string
	AppID        string
	ClientID     string
	ClientSecret string
	// RedirectURL is the login callback registered with KKU — echoed back on
	// the token exchange, see package doc.
	RedirectURL string
	HTTP        *http.Client
}

// Identity is what /auth.token returns alongside the access token. Note the
// absence of citizenId — see package doc.
type Identity struct {
	AccessToken string `json:"accessToken"`
	Email       string `json:"email"`
	ImmutableID string `json:"immutableId"`
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	EmployeeID  string `json:"employeeId"`
}

// Profile is the subset of /user.profile we have a use for. phoneNumber and
// gender are intentionally not here — see package doc.
type Profile struct {
	Email            string `json:"email"`
	UserID           string `json:"userId"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	FirstName        string `json:"firstname"`
	LastName         string `json:"lastname"`
	TitleEng         string `json:"titleEng"`
	FirstNameEng     string `json:"firstnameEng"`
	LastNameEng      string `json:"lastnameEng"`
	FacultyName      string `json:"facultyName"`
	PositionName     string `json:"positionName"`
	PositionTypeName string `json:"positionTypeName"`
	LevelID          string `json:"levelId"`
	LevelName        string `json:"levelName"`
	Workline         string `json:"workline"`
	PersonStatus     string `json:"personStatus"`
	LastVerify       string `json:"lastVerify"`
}

func (c *Client) loginBase() string {
	if c.LoginBase != "" {
		return strings.TrimRight(c.LoginBase, "/")
	}
	return DefaultLoginBase
}

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return DefaultAPIBase
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// LoginURL is where the browser is sent to start a login.
func (c *Client) LoginURL() string {
	return c.loginBase() + "/login?app=" + url.QueryEscape(c.AppID)
}

// LogoutURL ends the KKU-side session too. Without this a shared machine
// that "logged out" of our app still holds a live SSONext session, and the
// next person clicking "เข้าสู่ระบบด้วย KKU" is signed straight back in as
// the previous user without a password prompt.
func (c *Client) LogoutURL() string {
	return c.loginBase() + "/logout?app=" + url.QueryEscape(c.AppID)
}

// Exchange redeems the code SSONext appended to the callback URL.
func (c *Client) Exchange(ctx context.Context, code string) (*Identity, error) {
	if code == "" {
		return nil, ErrRejected
	}
	body, err := json.Marshal(map[string]string{
		"code":         code,
		"redirectUrl":  c.RedirectURL,
		"clientId":     c.ClientID,
		"clientSecret": c.ClientSecret,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+"/auth.token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("ssonext: auth.token: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("ssonext: auth.token: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ssonext: auth.token: HTTP %d", res.StatusCode)
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Identity
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ssonext: auth.token: bad JSON: %w", err)
	}
	if !out.OK {
		return nil, &RejectedError{Code: out.Error}
	}
	if out.AccessToken == "" || out.Email == "" {
		return nil, errors.New("ssonext: auth.token: ok without accessToken/email")
	}
	out.Email = strings.ToLower(strings.TrimSpace(out.Email))
	return &out.Identity, nil
}

// Profile fetches the extended record for an access token from Exchange.
// The login flow does not need it (Exchange already returns the email we
// match on); it exists for the follow-up of enriching a matched account
// with faculty / level / person type.
func (c *Client) Profile(ctx context.Context, accessToken string) (*Profile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+"/user.profile", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	res, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("ssonext: user.profile: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("ssonext: user.profile: %w", err)
	}
	if res.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ssonext: user.profile: HTTP %d", res.StatusCode)
	}
	var out struct {
		OK      bool    `json:"ok"`
		Profile Profile `json:"profile"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ssonext: user.profile: bad JSON: %w", err)
	}
	if !out.OK {
		return nil, ErrUnauthorized
	}
	out.Profile.Email = strings.ToLower(strings.TrimSpace(out.Profile.Email))
	return &out.Profile, nil
}
