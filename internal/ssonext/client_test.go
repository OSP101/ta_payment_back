package ssonext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeAPI is the minimum of ssonext-api.kku.ac.th needed to pin the wire
// format: JSON body on /auth.token, ok:false-as-200 for a rejection, and a
// Bearer-guarded /user.profile.
func fakeAPI(t *testing.T) (*httptest.Server, *map[string]string) {
	t.Helper()
	seen := map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth.token", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("auth.token body not JSON: %v", err)
		}
		for k, v := range in {
			seen[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		if in["code"] != "good-code" {
			_, _ = w.Write([]byte(`{"ok":false,"error":"AUTH0001"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"accessToken":"tok-1","email":" Somchai.J@KKUMAIL.COM ",
			"immutableId":"imm-1","citizenId":"1234567890123","firstName":"สมชาย","lastName":"ใจดี","employeeId":""}`))
	})
	mux.HandleFunc("/user.profile", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"profile":{"email":"somchai.j@kkumail.com","userId":"633020334-8",
			"type":"STUDENT","citizenId":"1234567890123","phoneNumber":"0812345678","gender":"M",
			"facultyName":"วิทยาลัยการคอมพิวเตอร์","levelName":"ปริญญาตรี"}}`))
	})
	return httptest.NewServer(mux), &seen
}

func TestExchange_SendsManualShapeAndNormalisesEmail(t *testing.T) {
	srv, seen := fakeAPI(t)
	defer srv.Close()
	c := &Client{APIBase: srv.URL, ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app/login/sso"}

	id, err := c.Exchange(context.Background(), "good-code")
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "somchai.j@kkumail.com" {
		t.Errorf("email not normalised: %q", id.Email)
	}
	if id.AccessToken != "tok-1" || id.FirstName != "สมชาย" {
		t.Errorf("identity = %+v", id)
	}
	// The manual's exact field names, including the registered redirect
	// echoed back — a typo here is a silent AUTH0001 in production.
	for k, want := range map[string]string{
		"code": "good-code", "redirectUrl": "https://app/login/sso", "clientId": "cid", "clientSecret": "sec",
	} {
		if (*seen)[k] != want {
			t.Errorf("auth.token body %s = %q, want %q", k, (*seen)[k], want)
		}
	}
}

func TestExchange_OKFalseIsRejected(t *testing.T) {
	srv, _ := fakeAPI(t)
	defer srv.Close()
	c := &Client{APIBase: srv.URL}

	_, err := c.Exchange(context.Background(), "stale")
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Code != "AUTH0001" {
		t.Errorf("service error code not carried: %v", err)
	}
}

func TestProfile_DropsPIIAndHonours401(t *testing.T) {
	srv, _ := fakeAPI(t)
	defer srv.Close()
	c := &Client{APIBase: srv.URL}

	p, err := c.Profile(context.Background(), "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != "633020334-8" || p.Type != "STUDENT" || p.FacultyName != "วิทยาลัยการคอมพิวเตอร์" {
		t.Errorf("profile = %+v", p)
	}
	// Belt and braces: the struct has no home for these, so a re-encode
	// must not contain them either.
	enc, _ := json.Marshal(p)
	for _, leak := range []string{"1234567890123", "0812345678", `"M"`} {
		if containsBytes(enc, leak) {
			t.Errorf("profile re-encode leaks %s: %s", leak, enc)
		}
	}

	if _, err := c.Profile(context.Background(), "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("bad token err = %v, want ErrUnauthorized", err)
	}
}

func TestLoginAndLogoutURLs(t *testing.T) {
	c := &Client{AppID: "app 1"}
	if got := c.LoginURL(); got != "https://ssonext.kku.ac.th/login?app=app+1" {
		t.Errorf("LoginURL = %s", got)
	}
	c.LoginBase = "http://localhost:8099/"
	if got := c.LogoutURL(); got != "http://localhost:8099/logout?app=app+1" {
		t.Errorf("LogoutURL = %s", got)
	}
}

func containsBytes(b []byte, s string) bool { return bytes.Contains(b, []byte(s)) }
