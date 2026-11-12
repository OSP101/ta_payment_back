package telemetry

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

var email = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9.-]+\.[a-z]{2,}`)
var credentialURL = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]+:[^\s/@]+@`)
var bearer = regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/=-]+`)
var jwt = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
var queryValues = regexp.MustCompile(`\?[^\s"\\]+`)
var bearerPath = regexp.MustCompile(`(/(?:p|public)/)[^\s"\\?]+`)
var digits = regexp.MustCompile(`\b\d[\d -]{8,}\d\b`)
var secretAssignment = regexp.MustCompile(`(?i)(password|passwd|secret|access_token|refresh_token|authorization|cookie|citizen_id|bank_account|passport)(["']?\s*[:=]\s*["']?)[^\s,"';]+`)

// ScrubText is defence in depth for old printf/error call sites. New telemetry
// should use fixed messages and explicitly selected fields.
func ScrubText(s string) string {
	s = queryValues.ReplaceAllString(s, "?[redacted]")
	s = bearerPath.ReplaceAllString(s, "${1}[redacted]")
	s = credentialURL.ReplaceAllString(s, "${1}[redacted]@")
	s = bearer.ReplaceAllString(s, "Bearer [redacted]")
	s = jwt.ReplaceAllString(s, "[token redacted]")
	s = email.ReplaceAllString(s, "[email redacted]")
	s = secretAssignment.ReplaceAllString(s, "${1}=[redacted]")
	// Preserve UUID tails and hex trace IDs, which are investigation keys.
	var out strings.Builder
	last := 0
	for _, loc := range digits.FindAllStringIndex(s, -1) {
		start, end := loc[0], loc[1]
		if start > 0 && (s[start-1] == '-' || s[start-1] >= 'a' && s[start-1] <= 'z' || s[start-1] >= 'A' && s[start-1] <= 'Z') {
			continue
		}
		if end < len(s) && (s[end] >= 'a' && s[end] <= 'z' || s[end] >= 'A' && s[end] <= 'Z') {
			continue
		}
		if len(strings.NewReplacer("-", "", " ", "").Replace(s[start:end])) < 10 {
			continue
		}
		out.WriteString(s[last:start])
		out.WriteString("[digits redacted]")
		last = end
	}
	if last > 0 {
		out.WriteString(s[last:])
		s = out.String()
	}
	if len(s) > 8192 {
		s = s[:8192] + " [truncated]"
	}
	return s
}

type requestKey struct{}

func WithRequest(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestKey{}, id)
}

// LogHandler applies the same scrubbing to slog and the legacy log bridge,
// then joins records to their request and active span without PII labels.
type LogHandler struct{ next slog.Handler }

func NewLogHandler(next slog.Handler) slog.Handler { return &LogHandler{next} }
func (h *LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}
func cleanAttr(a slog.Attr) slog.Attr {
	k := strings.ToLower(a.Key)
	for _, sensitive := range []string{"password", "secret", "token", "cookie", "authorization", "citizen", "passport", "bank_account", "request_body", "response_body"} {
		if strings.Contains(k, sensitive) {
			return slog.String(a.Key, "[redacted]")
		}
	}
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		original := a.Value.Group()
		attrs := make([]slog.Attr, len(original))
		for i := range original {
			attrs[i] = cleanAttr(original[i])
		}
		a.Value = slog.GroupValue(attrs...)
	} else if a.Value.Kind() == slog.KindString {
		a.Value = slog.StringValue(ScrubText(a.Value.String()))
	} else if a.Value.Kind() == slog.KindAny {
		a.Value = slog.StringValue(ScrubText(a.Value.String()))
	}
	return a
}
func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, ScrubText(r.Message), r.PC)
	hasRequestID := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "request_id" {
			hasRequestID = true
		}
		out.AddAttrs(cleanAttr(a))
		return true
	})
	parent := Parent(ctx)
	if id, ok := parent.Value(requestKey{}).(string); ok && !hasRequestID {
		out.AddAttrs(slog.String("request_id", id))
	}
	if tid, sid := TraceIDs(parent); tid != "" {
		out.AddAttrs(slog.String("trace_id", tid), slog.String("span_id", sid))
	}
	return h.next.Handle(ctx, out)
}
func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = cleanAttr(a)
	}
	return &LogHandler{h.next.WithAttrs(clean)}
}
func (h *LogHandler) WithGroup(name string) slog.Handler { return &LogHandler{h.next.WithGroup(name)} }
