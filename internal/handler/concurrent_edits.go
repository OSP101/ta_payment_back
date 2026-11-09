package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every authenticated API route passes through this guard AFTER its role
// checks. Versions are maintained by database triggers, not by the browser.
// No request payload, password, national ID or response body is retained.
var editConnections = make(chan struct{}, 8)

func ConcurrentEdits(pool *pgxpool.Pool, secret string) fiber.Handler {
	if secret == "" {
		secret = "test-only-edit-snapshots"
	}
	return func(c *fiber.Ctx) error {
		method := c.Method()
		actor := UserID(c)
		if actor == uuid.Nil || editReadOnlyCommand(c.Path()) {
			return c.Next()
		}
		primary, readScopes := editScopes(c, pool)
		if method == fiber.MethodGet || method == fiber.MethodHead {
			var snapshot string
			if err := pool.QueryRow(c.Context(), `SELECT pg_current_snapshot()::text`).Scan(&snapshot); err != nil {
				return err
			}
			err := c.Next()
			if err == nil && c.Response().StatusCode() < 300 {
				views := map[string]string{}
				for _, scope := range readScopes {
					views[scope] = signEditSnapshot(secret, actor, scope, snapshot)
				}
				raw, _ := json.Marshal(views)
				c.Set("X-Edit-Views", string(raw))
			}
			return err
		}
		if method != fiber.MethodPost && method != fiber.MethodPut && method != fiber.MethodPatch && method != fiber.MethodDelete {
			return c.Next()
		}
		// A separate session avoids exhausting the service pool: handlers acquire
		// their own connections while this session holds the advisory lock.
		select {
		case editConnections <- struct{}{}:
			defer func() { <-editConnections }()
		default:
			c.Set("Retry-After", "2")
			return fiber.NewError(fiber.StatusServiceUnavailable, "ระบบกำลังประมวลผล กรุณาลองใหม่สักครู่")
		}
		connectCtx, connectCancel := context.WithTimeout(c.Context(), 5*time.Second)
		conn, err := pgx.ConnectConfig(connectCtx, pool.Config().ConnConfig.Copy())
		connectCancel()
		if err != nil {
			return err
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = conn.Close(ctx)
		}()

		scopes := []string{primary}
		if strings.HasSuffix(c.Route().Path, "/worklog/approve-batch") {
			var body struct {
				IDs []uuid.UUID `json:"assignment_ids"`
			}
			if json.Unmarshal(c.Body(), &body) == nil && len(body.IDs) > 0 && len(body.IDs) <= 100 {
				rows, e := conn.Query(c.Context(), `SELECT DISTINCT 'course:'||sec.teaching_course_id::text FROM ta_request_assignments a JOIN sections sec ON sec.id=a.section_id WHERE a.id=ANY($1::uuid[]) ORDER BY 1`, body.IDs)
				if e != nil {
					return e
				}
				for rows.Next() {
					var scope string
					if e = rows.Scan(&scope); e != nil {
						rows.Close()
						return e
					}
					scopes = append(scopes, scope)
				}
				rows.Close()
				if e = rows.Err(); e != nil {
					return e
				}
			}
		}
		sort.Strings(scopes)
		// Closing the dedicated session always releases every acquired lock,
		// including a partially acquired multi-course batch.
		for _, scope := range scopes {
			var locked bool
			if err = conn.QueryRow(c.Context(), `SELECT pg_try_advisory_lock(hashtextextended($1,155))`, scope).Scan(&locked); err != nil {
				return err
			}
			if !locked {
				return fiber.NewError(409, "มีผู้กำลังดำเนินการกับรายการนี้ กรุณารอสักครู่แล้วโหลดข้อมูลล่าสุด")
			}
		}
		var input map[string]json.RawMessage
		_ = json.Unmarshal(c.Body(), &input)
		unsafe := method != fiber.MethodPost || len(input["id"]) > 0 || editTransition(c.Path()) || editExistingResource(c.Route().Path)
		if unsafe {
			for _, scope := range scopes {
				if err := checkEditSnapshot(c, conn, secret, actor, scope); err != nil {
					return err
				}
			}
		}
		command := uuid.Nil
		if raw := c.Get("Idempotency-Key"); raw != "" {
			command, err = uuid.Parse(raw)
			if err != nil {
				return fiber.NewError(400, "รหัสรายการไม่ถูกต้อง")
			}
			tag, e := conn.Exec(c.Context(), `INSERT INTO edit_commands(actor_id,command_id,method,path) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, actor, command, method, c.Path())
			if e != nil {
				return e
			}
			if tag.RowsAffected() == 0 {
				return fiber.NewError(409, "รายการนี้ถูกส่งไปแล้ว กรุณาโหลดข้อมูลล่าสุดเพื่อตรวจสอบผล")
			}
		}
		err = c.Next()
		status := c.Response().StatusCode()
		if command != uuid.Nil {
			if err == nil && status < 300 {
				_, e := conn.Exec(c.Context(), `UPDATE edit_commands SET completed=true WHERE actor_id=$1 AND command_id=$2`, actor, command)
				if e != nil {
					return e
				}
			} else if (err == nil && status < 500) || (editErrorStatus(err) >= 400 && editErrorStatus(err) < 500) {
				_, _ = conn.Exec(c.Context(), `DELETE FROM edit_commands WHERE actor_id=$1 AND command_id=$2`, actor, command)
			}
		}
		if err == nil && status < 300 {
			var snapshot string
			if e := conn.QueryRow(c.Context(), `SELECT pg_current_snapshot()::text`).Scan(&snapshot); e != nil {
				return e
			}
			responseScope := primary
			if strings.HasSuffix(primary, ":*") {
				var result struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(c.Response().Body(), &result)
				if _, e := uuid.Parse(result.ID); e == nil {
					responseScope = strings.TrimSuffix(primary, "*") + result.ID
				}
			}
			// A collection write cannot refresh views of other rows in that collection.
			views := map[string]string{}
			for _, scope := range scopes {
				if !strings.HasSuffix(scope, ":*") {
					views[scope] = signEditSnapshot(secret, actor, scope, snapshot)
				}
			}
			if !strings.HasSuffix(responseScope, ":*") {
				views[responseScope] = signEditSnapshot(secret, actor, responseScope, snapshot)
			}
			raw, _ := json.Marshal(views)
			c.Set("X-Edit-Views", string(raw))
		}
		return err
	}
}

func checkEditSnapshot(c *fiber.Ctx, conn *pgx.Conn, secret string, actor uuid.UUID, scope string) error {
	if len(c.Get("X-Edit-Views")) > 16384 {
		return fiber.NewError(400, "ข้อมูลตรวจสอบการแก้ไขมีขนาดใหญ่เกินไป")
	}
	var views map[string]string
	_ = json.Unmarshal([]byte(c.Get("X-Edit-Views")), &views)
	source := scope
	token := views[source]
	if token == "" {
		source = editCollection(scope)
		token = views[source]
	}
	snapshot, valid := verifyEditSnapshot(secret, actor, source, token)
	var tracked bool
	if err := conn.QueryRow(c.Context(), `SELECT EXISTS(SELECT 1 FROM edit_resource_versions WHERE scope=$1)`, scope).Scan(&tracked); err != nil {
		return err
	}
	if !tracked {
		return nil
	}
	if !valid {
		return fiber.NewError(428, "กรุณาโหลดข้อมูลล่าสุดก่อนบันทึกการเปลี่ยนแปลง")
	}
	var visible bool
	if err := conn.QueryRow(c.Context(), `SELECT pg_visible_in_snapshot(changed_by,$2::pg_snapshot) FROM edit_resource_versions WHERE scope=$1`, scope, snapshot).Scan(&visible); err != nil {
		return err
	}
	if !visible {
		return fiber.NewError(409, "ข้อมูลนี้มีการเปลี่ยนแปลงหลังจากเปิดหน้า กรุณารีเฟรชและตรวจสอบก่อนบันทึกอีกครั้ง")
	}
	return nil
}

func editErrorStatus(err error) int {
	if err == nil {
		return 0
	}
	status, _ := errorResponse(err)
	return status
}

// These POSTs only preview/reveal data, mint short-lived download tokens, or
// maintain session/read markers. Repeating them is intentional and safe.
func editReadOnlyCommand(path string) bool {
	for _, suffix := range []string{"/auth/heartbeat", "/me/citizen-id/reveal", "/me/creditor-form/preview.pdf", "/mail-settings/preview", "/mail-settings/check", "/announcements/preview-audience", "/zip-token", "/download-all-token", "/suppliers.xlsx", "/reprint", "/read", "/read-all", "/me/enrollment-scope"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func editExistingResource(path string) bool {
	// New children also change the parent being viewed: two users cannot append
	// against different versions of a timetable or a submitted request.
	if strings.Contains(path, ":") {
		return true
	}
	for _, suffix := range []string{"/settings/admin-officers", "/submission-periods", "/ta-request/windows", "/worklog/approve-batch", "/me/profile", "/me/schedule", "/me/documents", "/me/avatar", "/me/password", "/me/2fa/enable", "/me/2fa/disable", "/me/2fa/recovery-codes", "/me/creditor-form/confirm"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func editCollection(scope string) string {
	p, _, _ := strings.Cut(scope, ":")
	if p == "table" {
		return scope
	}
	return p + ":*"
}
func signEditSnapshot(secret string, actor uuid.UUID, scope, snapshot string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s|%s|%s", actor, scope, snapshot)
	return base64.RawURLEncoding.EncodeToString([]byte(snapshot)) + "." + hex.EncodeToString(mac.Sum(nil))
}
func verifyEditSnapshot(secret string, actor uuid.UUID, scope, token string) (string, bool) {
	encoded, _, ok := strings.Cut(token, ".")
	if !ok {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	snapshot := string(raw)
	return snapshot, hmac.Equal([]byte(token), []byte(signEditSnapshot(secret, actor, scope, snapshot)))
}
func editTransition(path string) bool {
	for _, s := range []string{"/staff-review", "/finance-send", "/send-back", "/finance-revert", "/cancel", "/publish", "/unpublish", "/approve-all", "/reject-batch", "/approve", "/reject", "/review-edit", "/review-cut", "/worklog-batch"} {
		if strings.HasSuffix(path, s) {
			return true
		}
	}
	return strings.Contains(path, "/document-progress/") && !strings.HasSuffix(path, "/share-link") && !strings.HasSuffix(path, "/remind")
}

func editScopes(c *fiber.Ctx, pool *pgxpool.Pool) (string, []string) {
	path := c.Route().Path
	// Strip the real or demo API prefix using the known route segments.
	parts := strings.Split(strings.Trim(path, "/"), "/")
	start := 0
	if len(parts) > 0 && parts[0] == "api" {
		start++
	}
	if start < len(parts) && parts[start] == "v1" {
		start++
	}
	if start < len(parts) && parts[start] == "demo" {
		start += 3
	} // demo/w/:slot
	if start >= len(parts) {
		return "table:unknown", []string{"table:unknown"}
	}
	parts = parts[start:]
	root := parts[0]
	id := func(names ...string) string {
		for _, n := range names {
			v := strings.TrimSuffix(c.Params(n), ".zip")
			if _, e := uuid.Parse(v); e == nil {
				return v
			}
		}
		return ""
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(c.Body(), &body)
	field := func(names ...string) string {
		for _, n := range names {
			var v string
			_ = json.Unmarshal(body[n], &v)
			if _, e := uuid.Parse(v); e == nil {
				return v
			}
		}
		return ""
	}
	query := func(names ...string) string {
		for _, n := range names {
			v := c.Query(n)
			if _, e := uuid.Parse(v); e == nil {
				return v
			}
		}
		return ""
	}
	lookup := func(sql, key string) string {
		if key == "" {
			return ""
		}
		var out string
		_ = pool.QueryRow(c.Context(), sql, key).Scan(&out)
		return out
	}
	course := field("teaching_course_id", "course_id")
	if course == "" {
		course = query("teaching_course_id", "tc_id", "course_id")
	}
	user := field("user_id", "ta_id")
	term := field("term_id")
	if term == "" {
		term = query("term_id")
	}
	table := strings.ReplaceAll(root, "-", "_")
	scope := "table:" + table
	switch root {
	case "teaching-courses":
		course = id("tcId", "id")
		if course == "" {
			course = field("id")
		}
		scope = "course:*"
	case "ta-requests":
		scope = "course:*"
		if id("id") != "" {
			course = lookup(`SELECT teaching_course_id::text FROM ta_requests WHERE id=$1`, id("id"))
		}
	case "assignments":
		course = lookup(`SELECT sec.teaching_course_id::text FROM ta_request_assignments a JOIN sections sec ON sec.id=a.section_id WHERE a.id=$1`, id("id"))
	case "worklogs":
		course = lookup(`SELECT sec.teaching_course_id::text FROM work_logs w JOIN ta_request_assignments a ON a.id=w.assignment_id JOIN sections sec ON sec.id=a.section_id WHERE w.id=$1`, id("logId"))
	case "staff":
		course = id("tcId")
		if strings.Contains(path, "data-deletion-requests") {
			user = lookup(`SELECT user_id::text FROM data_deletion_requests WHERE id=$1`, id("id"))
		}
		if strings.Contains(path, "/worklogs") {
			key := id("id")
			if key == "" {
				key = field("id")
			}
			if key != "" {
				course = lookup(`SELECT sec.teaching_course_id::text FROM work_logs w JOIN ta_request_assignments a ON a.id=w.assignment_id JOIN sections sec ON sec.id=a.section_id WHERE w.id=$1`, key)
			}
			if course == "" {
				course = lookup(`SELECT sec.teaching_course_id::text FROM ta_request_assignments a JOIN sections sec ON sec.id=a.section_id WHERE a.id=$1`, field("assignment_id"))
			}
		}
	case "sections":
		course = lookup(`SELECT teaching_course_id::text FROM sections WHERE id=$1`, id("id"))
	case "terms":
		term = id("id")
		if term == "" {
			term = field("id")
		}
		scope = "term:*"
	case "users":
		user = id("id")
		scope = "user:*"
	case "reports":
		scope = "course:*"
	case "me":
		user = UserID(c).String()
		if strings.Contains(path, "courses") || strings.Contains(path, "assignments") || strings.Contains(path, "ta-requests") {
			scope = "course:*"
			user = ""
			course = id("tcId")
		}
	case "ta-review":
		user = id("userId")
		if user == "" && strings.Contains(path, "/docs/") {
			user = lookup(`SELECT user_id::text FROM ta_documents WHERE id=$1`, id("id"))
		}
		scope = "user:*"
	case "submission-periods":
		course = id("tcId")
		if v := id("termId"); v != "" {
			term = v
		}
		scope = "course:*"
		if course == "" && term == "" {
			term = lookup(`SELECT term_id::text FROM submission_periods WHERE id=$1`, id("id"))
		}
		if course == "" && term == "" {
			term = field("term_id")
		}
	case "document-progress":
		if v := id("termId"); v != "" {
			term = v
		}
		course = id("tcId")
		scope = "term:*"
	case "exports":
		if strings.Contains(path, "/course/") {
			course = id("id")
		} else {
			if v := id("id"); v != "" {
				term = v
			}
			if term == "" {
				term = field("term_id")
			}
		}
	case "announcements":
		key := id("id")
		if key == "" {
			key = field("id")
		}
		scope = "announcement:*"
		if key != "" {
			scope = "announcement:" + key
		}
	case "curricula":
		scope = "curriculum:*"
		if c.Params("code") != "" {
			scope = "curriculum:" + c.Params("code")
		}
	case "holidays":
		scope = "table:public_holidays"
	case "mail-settings":
		scope = "table:mail_settings"
	case "settings":
		user = ""
		term = ""
		scope = "table:" + strings.ReplaceAll(parts[len(parts)-1], "-", "_")
		if strings.Contains(path, "pay-rate") {
			scope = "table:pay_rates"
		}
		if strings.Contains(path, "admin-officers") {
			scope = "table:admin_officers"
		}
	case "ta-request":
		term = ""
		scope = "table:ta_request_windows"
	}
	if term != "" {
		scope = "term:" + term
	}
	if user != "" {
		scope = "user:" + user
	}
	if course != "" {
		scope = "course:" + course
	}
	read := []string{scope}
	// Aggregate review screens read course data across a term; the token is
	// checked against the individual course's transaction when it is edited.
	if c.Method() == fiber.MethodGet && (root == "worklog" || root == "submission-periods" || root == "ta-requests" || root == "exports" || root == "document-progress" || root == "terms" || root == "staff") {
		read = append(read, "course:*")
	}
	if c.Method() == fiber.MethodGet && (root == "ta-request" || root == "submission-periods") {
		read = append(read, "term:*")
	}
	sort.Strings(read)
	return scope, read
}
