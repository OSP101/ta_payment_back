package handler

import (
	"strings"
)

// contentDisposition builds a Content-Disposition header value that survives a
// non-ASCII filename.
//
// HTTP header values are ISO-8859-1 (RFC 9110 §5.5), so raw UTF-8 bytes written
// straight into `filename="…"` are reinterpreted one byte at a time. Every Thai
// filename arrived mangled: `633020334-8_สุพพิธาน_ภักสวัสดิ์.pdf` reached the
// browser as `633020334-8_*8\x1e\x1e4\x182\x19_ 1\x01*'1*\x144L.pdf`, and the
// save dialog offered that as the name.
//
// RFC 6266 §4.1 is the fix, and it needs BOTH parameters:
//
//	filename="…"          ASCII only — what a client that knows nothing else uses
//	filename*=UTF-8''…    percent-encoded UTF-8 — what every current browser prefers
//
// Order matters: `filename` comes first so a naive parser that stops at the
// first match still gets something usable.
//
// This also closes a header-injection route. `filename` for an uploaded
// document is a string the client chose, and a CR or LF in it would let the
// uploader append headers of their own. Both encodings below emit only safe
// bytes, so that cannot happen regardless of what was uploaded.
func contentDisposition(kind, filename string) string {
	return kind +
		`; filename="` + asciiFilenameFallback(filename) + `"` +
		`; filename*=UTF-8''` + rfc5987Escape(filename)
}

// rfc5987Escape percent-encodes every byte outside the unreserved ASCII set.
//
// Deliberately stricter than url.PathEscape, which leaves `;` `,` `&` and `=`
// alone — inside a header parameter those terminate the value, so a filename
// containing one would truncate the name or, worse, look like another
// parameter. Encoding byte-wise (not rune-wise) is what makes the result valid
// UTF-8 percent-encoding.
func rfc5987Escape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) * 3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0F])
	}
	return b.String()
}

// asciiFilenameFallback reduces a filename to safe ASCII for the plain
// `filename` parameter.
//
// Each run of unrepresentable characters collapses to a single underscore
// rather than one per byte — a Thai name is three bytes per character, so
// per-byte substitution would turn `จุฑามาศ` into twenty-one underscores. The
// student ID and the extension survive, which is what makes the fallback
// recognisable at all: `123456789-0_จุฑามาศ_ชะรานันท์.pdf` → `123456789-0_.pdf`.
func asciiFilenameFallback(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingUnderscore := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '-', r == '.':
			b.WriteRune(r)
			pendingUnderscore = false
		default:
			// A literal '_' lands here too, on purpose: it has to collapse
			// together with the substituted ones, or `_จุฑามาศ_` becomes
			// separator + substitute + separator = three underscores.
			if !pendingUnderscore {
				b.WriteByte('_')
				pendingUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	// A name that reduced to nothing (or to a bare extension, which is hidden
	// on Unix and rejected by some download handlers) needs a real stand-in.
	if out == "" || strings.HasPrefix(out, ".") {
		return "download" + out
	}
	return out
}
