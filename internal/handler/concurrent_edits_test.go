package handler

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/testutil"
)

func TestEditSnapshotRejectsOtherDeviceAndBackgroundChanges(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	actor := uuid.New()
	target := uuid.New()
	for _, id := range []uuid.UUID{actor, target} {
		if _, e := pool.Exec(ctx, `INSERT INTO users(id,email,first_name,last_name) VALUES($1,$2,'old','test')`, id, id.String()+"@example.test"); e != nil {
			t.Fatal(e)
		}
	}
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c *fiber.Ctx) error { c.Locals(string(CtxUserID), actor); return c.Next() })
	guard := ConcurrentEdits(pool, "test-secret")
	app.Get("/api/v1/users/:id", guard, func(c *fiber.Ctx) error {
		var name string
		if e := pool.QueryRow(c.Context(), `SELECT first_name FROM users WHERE id=$1`, c.Params("id")).Scan(&name); e != nil {
			return e
		}
		return c.JSON(fiber.Map{"first_name": name})
	})
	app.Patch("/api/v1/users/:id", guard, func(c *fiber.Ctx) error {
		_, e := pool.Exec(c.Context(), `UPDATE users SET first_name=$2 WHERE id=$1`, c.Params("id"), string(c.Body()))
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"ok": true})
	})
	path := "/api/v1/users/" + target.String()
	read := func() string {
		t.Helper()
		r, e := app.Test(httptest.NewRequest("GET", path, nil))
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		return r.Header.Get("X-Edit-Views")
	}
	write := func(view, value string) int {
		t.Helper()
		q := httptest.NewRequest("PATCH", path, strings.NewReader(value))
		q.Header.Set("X-Edit-Views", view)
		r, e := app.Test(q)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		return r.StatusCode
	}
	deviceA, deviceB := read(), read()
	if status := write(deviceA, "first"); status != 200 {
		t.Fatal(status)
	}
	if status := write(deviceB, "stale"); status != 409 {
		t.Fatalf("stale device status=%d", status)
	}
	fresh := read()
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `UPDATE users SET first_name='background' WHERE id=$1`, target); e != nil {
		t.Fatal(e)
	}
	during := read() // writer is active in this snapshot even though its XID is older
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	for _, view := range []string{fresh, during} {
		if status := write(view, "overwrite"); status != 409 {
			t.Fatalf("background change not detected: %d", status)
		}
	}
	if status := write("{}", "missing snapshot"); status != 428 {
		t.Fatal(status)
	}
	var tokens map[string]string
	_ = json.Unmarshal([]byte(read()), &tokens)
	for scope := range tokens {
		tokens[scope] += "tampered"
	}
	bad, _ := json.Marshal(tokens)
	if status := write(string(bad), "tampered"); status != 428 {
		t.Fatal(status)
	}
	if status := write(read(), "refreshed"); status != 200 {
		t.Fatal(status)
	}
}

func TestCommandReceiptPreventsDuplicatePostWithoutStoringPayload(t *testing.T) {
	pool := testutil.NewPool(t)
	actor := uuid.New()
	if _, e := pool.Exec(context.Background(), `INSERT INTO users(id,email,first_name,last_name) VALUES($1,$2,'test','actor')`, actor, actor.String()+"@example.test"); e != nil {
		t.Fatal(e)
	}
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c *fiber.Ctx) error { c.Locals(string(CtxUserID), actor); return c.Next() })
	called := 0
	app.Post("/api/v1/announcements", ConcurrentEdits(pool, "secret"), func(c *fiber.Ctx) error { called++; return c.JSON(fiber.Map{"ok": true}) })
	key := uuid.NewString()
	for i, want := range []int{200, 409} {
		req := httptest.NewRequest("POST", "/api/v1/announcements", strings.NewReader(`{"title":"same intent"}`))
		req.Header.Set("Idempotency-Key", key)
		res, e := app.Test(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("call%d=%d want%d", i, res.StatusCode, want)
		}
	}
	if called != 1 {
		t.Fatalf("handler executed %d times", called)
	}
}

func TestConcurrentEditRoutesStayCovered(t *testing.T) {
	file, e := parser.ParseFile(token.NewFileSet(), "router.go", nil, 0)
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		target, ok := sel.X.(*ast.Ident)
		if !ok || target.Name != "authed" {
			return true
		}
		switch sel.Sel.Name {
		case "Get", "Post", "Put", "Patch", "Delete":
		default:
			return true
		}
		count++
		found := false
		for _, arg := range call.Args {
			if id, ok := arg.(*ast.Ident); ok && id.Name == "editGuard" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s route misses editGuard: %v", sel.Sel.Name, call.Args[0])
		}
		return true
	})
	if count < 200 {
		t.Fatalf("unexpected route inventory: %d", count)
	}
	t.Logf("checked %d authenticated read/write routes", count)
}

func TestConcurrentGuardRejectsInFlightOtherDevice(t *testing.T) {
	pool := testutil.NewPool(t)
	actor := uuid.New()
	if _, e := pool.Exec(context.Background(), `INSERT INTO users(id,email,first_name,last_name) VALUES($1,$2,'test','actor')`, actor, actor.String()+"@example.test"); e != nil {
		t.Fatal(e)
	}
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c *fiber.Ctx) error { c.Locals(string(CtxUserID), actor); return c.Next() })
	entered, release := make(chan struct{}), make(chan struct{})
	app.Post("/api/v1/announcements", ConcurrentEdits(pool, "secret"), func(c *fiber.Ctx) error { close(entered); <-release; return c.SendStatus(200) })
	done := make(chan int, 1)
	go func() {
		r, e := app.Test(httptest.NewRequest("POST", "/api/v1/announcements", nil), 5000)
		if e != nil {
			done <- 0
			return
		}
		r.Body.Close()
		done <- r.StatusCode
	}()
	<-entered
	r, e := app.Test(httptest.NewRequest("POST", "/api/v1/announcements", nil))
	if e != nil {
		close(release)
		t.Fatal(e)
	}
	r.Body.Close()
	close(release)
	if r.StatusCode != 409 {
		t.Fatalf("simultaneous write=%d", r.StatusCode)
	}
	if status := <-done; status != 200 {
		t.Fatalf("first write=%d", status)
	}
}

func TestRejectedValidationCanRetryTheSameCommand(t *testing.T) {
	pool := testutil.NewPool(t)
	actor := uuid.New()
	if _, e := pool.Exec(context.Background(), `INSERT INTO users(id,email,first_name,last_name) VALUES($1,$2,'test','actor')`, actor, actor.String()+"@example.test"); e != nil {
		t.Fatal(e)
	}
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c *fiber.Ctx) error { c.Locals(string(CtxUserID), actor); return c.Next() })
	calls := 0
	app.Post("/api/v1/announcements", ConcurrentEdits(pool, "secret"), func(c *fiber.Ctx) error {
		calls++
		if calls == 1 {
			return fiber.NewError(422, "validation")
		}
		if calls == 2 {
			return service.Invalid("ข้อมูลไม่ถูกต้อง")
		}
		return c.SendStatus(200)
	})
	key := uuid.NewString()
	for _, want := range []int{422, 400, 200, 409} {
		q := httptest.NewRequest("POST", "/api/v1/announcements", nil)
		q.Header.Set("Idempotency-Key", key)
		r, e := app.Test(q)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Fatalf("got%d want%d", r.StatusCode, want)
		}
	}
}

func TestEditScopeMappingsAcrossPageFamilies(t *testing.T) {
	pool := testutil.NewPool(t)
	actor := uuid.New()
	course := uuid.NewString()
	term := uuid.NewString()
	user := uuid.NewString()
	cases := []struct{ method, path, url, body, want string }{
		{"GET", "/api/v1/staff/courses/:tcId/worklogs", "/api/v1/staff/courses/" + course + "/worklogs", "", "course:" + course},
		{"GET", "/api/v1/reports/pending", "/api/v1/reports/pending", "", "course:*"},
		{"GET", "/api/v1/me/assignments", "/api/v1/me/assignments", "", "course:*"},
		{"PUT", "/api/v1/me/profile", "/api/v1/me/profile", "{}", "user:" + actor.String()},
		{"PATCH", "/api/v1/users/:id", "/api/v1/users/" + user, "{}", "user:" + user},
		{"POST", "/api/v1/ta-review/:userId/profile", "/api/v1/ta-review/" + user + "/profile", "{}", "user:" + user},
		{"PATCH", "/api/v1/teaching-courses/:id/settings", "/api/v1/teaching-courses/" + course + "/settings", "{}", "course:" + course},
		{"POST", "/api/v1/ta-requests", "/api/v1/ta-requests", `{"teaching_course_id":"` + course + `"}`, "course:" + course},
		{"GET", "/api/v1/submission-periods", "/api/v1/submission-periods?term_id=" + term, "", "term:" + term},
		{"POST", "/api/v1/submission-periods", "/api/v1/submission-periods", `{"term_id":"` + term + `"}`, "term:" + term},
		{"POST", "/api/v1/ta-request/windows", "/api/v1/ta-request/windows", `{"term_id":"` + term + `"}`, "table:ta_request_windows"},
		{"POST", "/api/v1/document-progress/:termId", "/api/v1/document-progress/" + term, "{}", "term:" + term},
		{"POST", "/api/v1/document-progress/checklist/:tcId", "/api/v1/document-progress/checklist/" + course, "{}", "course:" + course},
		{"POST", "/api/v1/exports/terms/:id/monthly-payout.zip", "/api/v1/exports/terms/" + term + "/monthly-payout.zip", "{}", "term:" + term},
		{"POST", "/api/v1/settings/admin-officers", "/api/v1/settings/admin-officers", `{"user_id":"` + user + `"}`, "table:admin_officers"},
		{"PATCH", "/api/v1/curricula/:code", "/api/v1/curricula/CS", "{}", "curriculum:CS"},
		{"GET", "/api/v1/curricula", "/api/v1/curricula", "", "curriculum:*"},
		{"POST", "/api/demo/w/:slot/teaching-courses/:id/sections", "/api/demo/w/1/teaching-courses/" + course + "/sections", "{}", "course:" + course},
	}
	for _, tt := range cases {
		t.Run(tt.url, func(t *testing.T) {
			app := fiber.New()
			app.Use(func(c *fiber.Ctx) error { c.Locals(string(CtxUserID), actor); return c.Next() })
			app.Add(tt.method, tt.path, func(c *fiber.Ctx) error {
				got, _ := editScopes(c, pool)
				if got != tt.want {
					t.Errorf("scope=%q want%q", got, tt.want)
				}
				return c.SendStatus(200)
			})
			q := httptest.NewRequest(tt.method, tt.url, strings.NewReader(tt.body))
			q.Header.Set("Content-Type", "application/json")
			r, e := app.Test(q)
			if e != nil {
				t.Fatal(e)
			}
			r.Body.Close()
		})
	}
}

func TestEditTriggersTrackCatalogAndTermChanges(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx := context.Background()
	term := uuid.New()
	user := uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := pool.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO users(id,email,first_name,last_name) VALUES($1,$2,'test','actor')`, user, user.String()+"@example.test")
	exec(`INSERT INTO academic_terms(id,academic_year,semester) VALUES($1,2570,1)`, term)
	exec(`INSERT INTO ta_request_windows(term_id,opens_at,closes_at) VALUES($1,now(),now()+interval '1 day')`, term)
	exec(`UPDATE admin_officers SET user_id=$1 WHERE id=(SELECT id FROM admin_officers LIMIT 1)`, user)
	for _, scope := range []string{"user:" + user.String(), "term:" + term.String(), "table:ta_request_windows", "table:admin_officers"} {
		var present bool
		if e := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM edit_resource_versions WHERE scope=$1)`, scope).Scan(&present); e != nil {
			t.Fatal(e)
		}
		if !present {
			t.Errorf("missing trigger scope %s", scope)
		}
	}
}
