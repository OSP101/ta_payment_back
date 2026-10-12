package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/gofiber/fiber/v2"

	"ta-payment-back/internal/i18n"
)

// Locale is the reader's interface language for this request: the ui_locale
// cookie the web app keeps in step with the account (see the frontend's
// PreferenceSync), or an X-UI-Locale header for callers without cookies.
// Anything else is Thai.
func Locale(c *fiber.Ctx) string {
	l := c.Cookies("ui_locale")
	if l == "" {
		l = c.Get("X-UI-Locale")
	}
	if l == "en" {
		return "en"
	}
	return "th"
}

// TranslateErrors puts {"error": "…"} bodies that a handler or middleware
// wrote ITSELF (rate limits, guards) into the reader's language. Errors that
// are returned go through ErrorHandler instead, which translates its own.
func TranslateErrors() fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := c.Next()
		if err != nil || Locale(c) != "en" || c.Response().StatusCode() < 400 {
			return err
		}
		if string(c.Response().Header.ContentType()) != fiber.MIMEApplicationJSON &&
			string(c.Response().Header.ContentType()) != fiber.MIMEApplicationJSONCharsetUTF8 {
			return nil
		}
		var body map[string]any
		if json.Unmarshal(c.Response().Body(), &body) != nil {
			return nil
		}
		msg, ok := body["error"].(string)
		if !ok {
			return nil
		}
		if en := i18n.T("en", msg); en != msg {
			body["error"] = en
			if b, err := json.Marshal(body); err == nil {
				c.Response().SetBodyRaw(b)
			}
		}
		return nil
	}
}

// localizeFields translates, in place, the string fields of v whose JSON name
// is listed — descriptive wording the API derives (labels, details), never
// data such as names. v must be a pointer; slices, maps' values, nested
// structs and []string fields are walked.
func localizeFields(loc string, v any, jsonNames ...string) {
	if loc != "en" {
		return
	}
	want := map[string]bool{}
	for _, n := range jsonNames {
		want[n] = true
	}
	walkLocalize(reflect.ValueOf(v), want, false)
}

func walkLocalize(v reflect.Value, want map[string]bool, translate bool) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkLocalize(v.Elem(), want, translate)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkLocalize(v.Index(i), want, translate)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			e := v.MapIndex(k)
			if e.Kind() == reflect.Interface && !e.IsNil() && e.Elem().Kind() != reflect.String {
				walkLocalize(e.Elem(), want, translate)
				continue
			}
			if want[fmt.Sprint(k.Interface())] && e.Kind() == reflect.Interface && !e.IsNil() && e.Elem().Kind() == reflect.String {
				v.SetMapIndex(k, reflect.ValueOf(i18n.T("en", e.Elem().String())))
			}
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if f.Anonymous && name == "" {
				walkLocalize(v.Field(i), want, translate)
				continue
			}
			walkLocalize(v.Field(i), want, want[name])
		}
	case reflect.String:
		if translate && v.CanSet() {
			v.SetString(i18n.T("en", v.String()))
		}
	}
}

// translatedKeys are the JSON fields, in any response, whose values are
// wording the API composed (labels, reasons, warnings) rather than data. For
// an English reader TranslateFields runs them through i18n.T: catalog
// matches and Thai month names become English, anything else — a name, a
// note somebody typed — comes back unchanged. Never add a field the web app
// compares against a Thai value (e.g. "seniority"), or that comparison breaks,
// and never one a form can send back (a user's "title" prefix, a "note"): an
// English copy would be saved over the Thai.
var translatedKeys = map[string]bool{
	"label": true, "labels": true, "period_label": true, "round_label": true, "stage_label": true,
	"month_label": true, "scope_label": true, "status_label": true, "category_label": true, "term_label": true,
	"reason": true, "reasons": true, "state_reason": true, "lock_reason": true, "empty_reason": true,
	"message": true, "messages": true, "warning": true, "warnings": true, "issue": true, "issues": true,
	"problems": true, "detail": true, "details": true, "advice": true, "profile_issue": true,
	"missing": true, "incomplete": true, "level_th": true, "track_th": true, "ta_level_th": true,
	"claim_kind": true, "device": true, "network": true, "errors": true, "notice": true, "notices": true,
	"hint": true, "blockers": true, "headline": true,
	"fatal_error": true, "skip_reason": true, "action_label": true,
	"month_labels": true, "error": true, "last_error": true,
}

// TranslateFields localizes translatedKeys in successful JSON responses for
// English readers. Thai readers' responses are untouched (and not even parsed).
func TranslateFields() fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := c.Next()
		if err != nil || Locale(c) != "en" {
			return err
		}
		res := c.Response()
		if res.StatusCode() >= 300 || !strings.HasPrefix(string(res.Header.ContentType()), fiber.MIMEApplicationJSON) {
			return nil
		}
		body := res.Body()
		if len(body) == 0 || len(body) > 8<<20 || !hasThaiBytes(body) {
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			return nil
		}
		if !translateTree(v, false) {
			return nil
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if enc.Encode(v) == nil {
			res.SetBodyRaw(bytes.TrimRight(buf.Bytes(), "\n"))
		}
		return nil
	}
}

// translateTree walks decoded JSON; on=true means "the value sits under a
// translated key". Reports whether anything changed.
func translateTree(v any, on bool) bool {
	changed := false
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok {
				// on: this map sits under a translated key ("month_labels":
				// {"2026-06": "มิถุนายน"}), so every value is wording.
				if on || translatedKeys[k] {
					if t := i18n.T("en", s); t != s {
						x[k] = t
						changed = true
					}
				}
				continue
			}
			if translateTree(e, translatedKeys[k]) {
				changed = true
			}
		}
	case []any:
		for i, e := range x {
			if s, ok := e.(string); ok {
				if on {
					if t := i18n.T("en", s); t != s {
						x[i] = t
						changed = true
					}
				}
				continue
			}
			if translateTree(e, false) {
				changed = true
			}
		}
	}
	return changed
}

func hasThaiBytes(b []byte) bool {
	// Thai is U+0E00–U+0E7F: in UTF-8, 0xE0 0xB8 xx or 0xE0 0xB9 xx.
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0xE0 && (b[i+1] == 0xB8 || b[i+1] == 0xB9) {
			return true
		}
	}
	return false
}
