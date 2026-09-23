// ssonext-mock stands in for ssonext.kku.ac.th + ssonext-api.kku.ac.th on a
// dev machine, following คู่มือการใช้งาน KKU Single Sign On (SSONext) so the
// real service can be dropped in by changing four env vars. It has no idea
// who the real users are: the "login page" is a plain email box, and
// whatever address is typed is who KKU says you are. Point the backend at
// it with
//
//	SSO_APP_ID=mock SSO_CLIENT_ID=mock SSO_CLIENT_SECRET=mock
//	SSO_LOGIN_BASE=http://localhost:8099 SSO_API_BASE=http://localhost:8099
//
// and run `go run ./cmd/ssonext-mock`. Deliberately not wired into the real
// server binary: nothing here must ever be reachable in a deployment.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	addr           = flag.String("addr", ":8099", "listen address")
	redirect       = flag.String("redirect", "http://localhost:3000/login/sso", "registered login callback (redirectUrl must match)")
	logoutRedirect = flag.String("logout-redirect", "http://localhost:3000/login", "registered logout callback")
	appID          = flag.String("app", "mock", "App ID the login/logout links must carry")
	clientID       = flag.String("client-id", "mock", "expected clientId")
	clientSecret   = flag.String("client-secret", "mock", "expected clientSecret")
)

type identity struct {
	email, first, last string
}

var (
	mu     sync.Mutex
	codes  = map[string]identity{} // one-shot, like the real thing
	tokens = map[string]identity{}
)

func randHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	flag.Parse()
	mux := http.NewServeMux()

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("app") != *appID {
			http.Error(w, "unknown app", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPost {
			email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
			if email == "" {
				http.Redirect(w, r, r.URL.String(), http.StatusSeeOther)
				return
			}
			code := randHex()
			mu.Lock()
			codes[code] = identity{email: email, first: r.FormValue("first"), last: r.FormValue("last")}
			mu.Unlock()
			time.AfterFunc(2*time.Minute, func() { mu.Lock(); delete(codes, code); mu.Unlock() })
			// The real service adds ssoState=code too (seen on reg2's
			// callback); mirrored so the callback page proves it copes.
			http.Redirect(w, r, *redirect+"?code="+url.QueryEscape(code)+"&ssoState=code", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>SSONext mock</title>
<body style="font-family:sans-serif;max-width:420px;margin:60px auto">
<h2>KKU SSONext <small style="color:#999">(mock)</small></h2>
<p>ใส่อีเมล KKU ของบัญชีที่มีอยู่ในระบบ แล้วกดเข้าสู่ระบบ — จะเด้งกลับไปที่<br><code>%s</code></p>
<form method="post">
<label>Email <input name="email" type="email" required autofocus style="width:100%%"></label><br><br>
<label>ชื่อ <input name="first" value="ทดสอบ"></label> <label>สกุล <input name="last" value="ระบบ"></label><br><br>
<button type="submit">เข้าสู่ระบบ</button>
</form></body>`, html.EscapeString(*redirect))
	})

	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("app") != *appID {
			http.Error(w, "unknown app", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, *logoutRedirect, http.StatusFound)
	})

	mux.HandleFunc("/auth.token", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Code, RedirectUrl, ClientId, ClientSecret string }
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&in) != nil {
			writeJSON(w, map[string]any{"ok": false, "error": "AUTH0000"})
			return
		}
		mu.Lock()
		id, ok := codes[in.Code]
		delete(codes, in.Code)
		mu.Unlock()
		if !ok || in.ClientId != *clientID || in.ClientSecret != *clientSecret || in.RedirectUrl != *redirect {
			log.Printf("auth.token rejected: code known=%v client=%q redirect=%q", ok, in.ClientId, in.RedirectUrl)
			writeJSON(w, map[string]any{"ok": false, "error": "AUTH0001"})
			return
		}
		tok := randHex()
		mu.Lock()
		tokens[tok] = id
		mu.Unlock()
		writeJSON(w, map[string]any{
			"ok": true, "accessToken": tok, "email": id.email, "immutableId": "mock-" + id.email,
			"citizenId": "0000000000000", "firstName": id.first, "lastName": id.last, "employeeId": "",
		})
	})

	mux.HandleFunc("/user.profile", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		id, ok := tokens[tok]
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		typ := "STAFF"
		if strings.HasSuffix(id.email, "@kkumail.com") {
			typ = "STUDENT"
		}
		writeJSON(w, map[string]any{"ok": true, "profile": map[string]any{
			"email": id.email, "userId": "000000000-0", "type": typ, "citizenId": "0000000000000",
			"title": "", "firstname": id.first, "lastname": id.last, "titleEng": "", "firstnameEng": "", "lastnameEng": "",
			"facultyName": "วิทยาลัยการคอมพิวเตอร์", "positionName": "", "positionTypeName": "", "levelId": "", "levelName": "",
			"gender": "", "workline": "", "phoneNumber": "", "personStatus": "", "lastVerify": time.Now().Format(time.RFC3339),
		}})
	})

	mux.HandleFunc("/auth.status", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		id, ok := tokens[tok]
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "user": map[string]any{"sessionId": tok, "email": id.email, "role": "STAFF"}})
	})

	log.Printf("ssonext-mock on %s → login callback %s", *addr, *redirect)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
