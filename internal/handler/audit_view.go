package handler

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"ta-payment-back/internal/audit"
	"ta-payment-back/internal/service"
	"ta-payment-back/internal/timeutil"
)

// auditQueryFrom reads one question about the trail off the query string. The
// list and the export share it so that "export what I am looking at" exports
// exactly that.
func auditQueryFrom(c *fiber.Ctx) (service.AuditQuery, error) {
	q := service.AuditQuery{
		Role:     c.Query("role"),
		Action:   c.Query("action"),
		Entity:   c.Query("entity"),
		EntityID: c.Query("entity_id"),
		IP:       c.Query("ip"),
		Category: c.Query("category"),
		Severity: c.Query("severity"),
		Outcome:  c.Query("outcome"),
		Q:        strings.TrimSpace(c.Query("q")),
		Fold:     c.Query("fold") == "1",
	}
	var err error
	if q.From, err = optTime(c.Query("from")); err != nil {
		return q, fiber.NewError(fiber.StatusBadRequest, "from ไม่ใช่วันที่ที่ถูกต้อง")
	}
	if q.To, err = optTime(c.Query("to")); err != nil {
		return q, fiber.NewError(fiber.StatusBadRequest, "to ไม่ใช่วันที่ที่ถูกต้อง")
	}
	if !q.From.IsZero() && !q.To.IsZero() && q.To.Before(q.From) {
		return q, fiber.NewError(fiber.StatusBadRequest, "ช่วงเวลาไม่ถูกต้อง: วันสิ้นสุดอยู่ก่อนวันเริ่ม")
	}
	if q.ActorID, err = optUUID(c.Query("actor_id")); err != nil {
		return q, fiber.NewError(fiber.StatusBadRequest, "actor_id ไม่ถูกต้อง")
	}
	if q.RequestID, err = optUUID(c.Query("request_id")); err != nil {
		return q, fiber.NewError(fiber.StatusBadRequest, "request_id ไม่ถูกต้อง")
	}
	if q.SessionID, err = optUUID(c.Query("session_id")); err != nil {
		return q, fiber.NewError(fiber.StatusBadRequest, "session_id ไม่ถูกต้อง")
	}
	q.Limit, _ = strconv.Atoi(c.Query("limit", "50"))
	q.Offset, _ = strconv.Atoi(c.Query("offset", "0"))
	return q, nil
}

// Catalog hands the screen the vocabulary: the categories, and every action
// with its wording, category, severity and outcome. The screen used to carry
// its own copy of this, which is how the two drifted.
func (h *AuditHandler) Catalog(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"categories": audit.Categories(),
		"actions":    audit.Catalog(),
	})
}

// Sessions lists who is signed in right now, on what and from where.
func (h *AuditHandler) Sessions(c *fiber.Ctx) error {
	items, err := h.Svc.Audit.ActiveSessions(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"items":   items,
		"current": SessionID(c),
	})
}

// Export writes the rows a query matches as a CSV a spreadsheet opens directly.
//
// It is the trail leaving the system, so it is itself recorded — with the
// question that was asked and how many rows left — and it is capped: the table
// only grows, and "everything" is not a file anybody can open. A question that
// matches more than the cap is refused, never cut short.
func (h *AuditHandler) Export(c *fiber.Ctx) error {
	q, err := auditQueryFrom(c)
	if err != nil {
		return err
	}
	q.Fold = false
	q.Offset = 0
	q.Limit = service.AuditExportMaxRows
	q.MaxLimit = service.AuditExportMaxRows

	items, total, err := h.Svc.Audit.ListAudit(c.Context(), q)
	if err != nil {
		return err
	}

	// Refuse rather than truncate. A file that silently stops at the cap looks
	// exactly like a complete one, and it will be read as one.
	if total > len(items) {
		return &service.UserError{Status: fiber.StatusBadRequest, Msg: fmt.Sprintf(
			"ผลลัพธ์มี %d แถว เกินจำนวนที่ส่งออกได้ต่อไฟล์ (%d แถว) กรุณาย่อช่วงเวลาหรือเพิ่มตัวกรองแล้วลองใหม่",
			total, service.AuditExportMaxRows)}
	}

	actor := UserID(c)
	note := fmt.Sprintf("rows=%d", len(items))
	if subject := auditLookupSubject(q); subject != "" {
		note += " " + subject
	}
	// Recorded BEFORE the file is sent, and a failure to record refuses the
	// export: a copy of the trail that left without a trace is the one thing
	// this table must never allow.
	if err := h.Svc.Auditor.Log(c.Context(), audit.Entry{
		ActorID: &actor, Action: "audit_log.export", Entity: "audit_log", Note: note,
	}); err != nil {
		return err
	}

	var buf bytes.Buffer
	// The byte-order mark is what makes Excel read the file as UTF-8 rather
	// than as the machine's legacy Thai code page.
	buf.WriteString("\xEF\xBB\xBF")
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{
		"เลขอ้างอิง", "วันที่", "เวลา", "ผู้กระทำ", "บทบาท", "เหตุการณ์", "เกี่ยวกับ", "ผล", "ระดับ",
		"หมวด", "รายละเอียด", "สิ่งที่เปลี่ยน", "อุปกรณ์", "IP",
		"action", "entity", "entity_id", "request_id", "session_id", "method", "path", "note",
	})
	for i := range items {
		r := &items[i]
		at := r.At.In(timeutil.Bangkok)
		var changes []string
		for _, ch := range r.Changes {
			switch {
			case ch.Before != nil && ch.After != nil:
				changes = append(changes, fmt.Sprintf("%s: %s → %s", ch.Label, *ch.Before, *ch.After))
			case ch.After != nil:
				changes = append(changes, fmt.Sprintf("%s: %s", ch.Label, *ch.After))
			case ch.Before != nil:
				changes = append(changes, fmt.Sprintf("%s: %s (ก่อนลบ)", ch.Label, *ch.Before))
			}
		}
		_ = w.Write(csvSafeRow([]string{
			r.Ref,
			fmt.Sprintf("%02d/%02d/%d", at.Day(), int(at.Month()), at.Year()+543),
			at.Format("15:04:05"),
			auditActorText(r), auditRoleText(r.ActorRole), r.Label, r.SubjectName,
			auditOutcomeText(r.Outcome), auditSeverityText(r.Severity), r.CategoryLabel,
			strings.Join(r.Details, "; "), strings.Join(changes, "; "),
			r.Device, deref(r.IP),
			r.Action, r.Entity, deref(r.EntityID),
			uuidText(r.RequestID), uuidText(r.SessionID), deref(r.Method), deref(r.Path), deref(r.Note),
		}))
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}

	name := "audit-log-" + time.Now().In(timeutil.Bangkok).Format("20060102-1504") + ".csv"
	c.Set(fiber.HeaderContentType, "text/csv; charset=utf-8")
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="`+name+`"`)
	return c.Send(buf.Bytes())
}

// csvSafeRow defuses cells a spreadsheet would execute. Notes, names and paths
// are all text somebody else typed; a cell that begins with = + - or @ is a
// formula to Excel, and this file is opened by exactly the people whose
// machines matter. The leading apostrophe makes it text and is not displayed.
func csvSafeRow(cells []string) []string {
	for i, cell := range cells {
		if cell == "" {
			continue
		}
		switch cell[0] {
		case '=', '+', '-', '@', '\t', '\r':
			cells[i] = "'" + cell
		}
	}
	return cells
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func uuidText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func auditActorText(r *service.AuditRow) string {
	switch r.ActorKind {
	case "user":
		if r.ActorName != "" {
			return r.ActorName
		}
		return uuidText(r.ActorID)
	case "system":
		return "ระบบอัตโนมัติ"
	case "anonymous":
		return "ผู้ที่ยังไม่ได้เข้าสู่ระบบ"
	}
	return "ไม่ได้บันทึกผู้กระทำ"
}

func auditRoleText(role *string) string {
	switch deref(role) {
	case "admin":
		return "ผู้ดูแลระบบ"
	case "staff":
		return "เจ้าหน้าที่"
	case "lecturer":
		return "อาจารย์"
	case "ta":
		return "ผู้ช่วยสอน"
	}
	return deref(role)
}

func auditOutcomeText(o string) string {
	switch o {
	case "failed":
		return "ไม่สำเร็จ"
	case "denied":
		return "ถูกปฏิเสธ"
	}
	return "สำเร็จ"
}

func auditSeverityText(s string) string {
	switch s {
	case "danger":
		return "สำคัญ"
	case "warn":
		return "ควรตรวจ"
	case "notice":
		return "ข้อมูลอ่อนไหว"
	}
	return "ปกติ"
}

// ── Refusals ────────────────────────────────────────────────────────────────

// deniedAuditWindow throttles the refusal trail the same way the read trail is
// throttled (see audit_read.go), and for the same reason: a screen that polls
// an endpoint its user may not call would otherwise write a row every few
// seconds into a table nothing can be removed from.
const deniedAuditWindow = time.Hour

type deniedAuditKey struct {
	actor  uuid.UUID
	method string
	route  string
}

var deniedAuditSeen sync.Map // deniedAuditKey -> time.Time

// AuditDenied records a signed-in user being refused for lack of permission.
//
// The trail recorded what people did and, since the read audit, what they
// looked at. It had no record at all of what they TRIED and were not allowed
// to do — a TA calling a staff endpoint, a staff account calling an admin one.
// Those are the rows that show somebody probing, and every system this one was
// compared against keeps them.
//
// Only 403. A 401 has no user to attribute it to (failed logins are already
// recorded by the auth service) and a 404 is not a refusal.
func AuditDenied(aud *audit.Auditor) fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := c.Next()
		if err == nil {
			return nil
		}
		// errorResponse, not the response status: fiber runs its ErrorHandler
		// after every middleware has returned, so the response still reads 200
		// here (see AccessLog).
		if status, _ := errorResponse(err); status != fiber.StatusForbidden {
			return err
		}
		actor := UserID(c)
		if actor == uuid.Nil {
			return err
		}
		route := c.Route().Path
		k := deniedAuditKey{actor: actor, method: c.Method(), route: route}
		if !deniedAuditDue(k, time.Now()) {
			return err
		}
		// Best-effort, like the read trail: the refusal has already been
		// decided and must reach the client as a 403 whatever happens here.
		if lerr := aud.Log(c.Context(), audit.Entry{
			ActorID: &actor, Action: "access.denied", Entity: "route",
			Note: "route=" + c.Method() + " " + route,
		}); lerr != nil {
			deniedAuditSeen.Delete(k)
			log.Printf("audit denied: recording refusal for %s on %s failed: %v", actor, route, lerr)
		}
		return err
	}
}

// deniedAuditDue reports whether this refusal should be recorded, and marks it.
func deniedAuditDue(k deniedAuditKey, now time.Time) bool {
	if prev, seen := deniedAuditSeen.Load(k); seen {
		if last, isTime := prev.(time.Time); isTime && now.Sub(last) < deniedAuditWindow {
			return false
		}
	}
	deniedAuditSeen.Store(k, now)
	return true
}

// SweepDeniedAudit drops throttle entries whose window has passed.
func SweepDeniedAudit(now time.Time) {
	deniedAuditSeen.Range(func(k, v any) bool {
		if last, isTime := v.(time.Time); isTime && now.Sub(last) >= deniedAuditWindow {
			deniedAuditSeen.Delete(k)
		}
		return true
	})
}
