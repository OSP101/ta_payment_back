// watermark_middleware.go implements the "ลายน้ำเอกสาร" safety guard from
// docs/PLAN-demo-sandbox.md §7: every document a demo workspace hands out
// must be unmistakable from a real one, even after it leaves the browser
// (saved to disk, forwarded by email, printed).
//
// This is deliberately a response-rewriting middleware rather than a change
// to internal/pdfgen, internal/docxgen, or the download handlers themselves
// — those are shared with production and stay untouched. Every download
// route this system has (creditor forms, appointment orders, exports,
// transfer cover, ...) already flows through this slot's own mounted copy
// of handler.MountAPI (see handlers.go), so wrapping that mount point once
// covers all of them without needing to know each one by name — including
// ones added after this file was written.
package demo

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"ta-payment-back/internal/watermark"
)

// English, not Thai — confirmed empirically (rendered, inspected by eye) that
// pdfcpu's watermark stamp (which internal/watermark uses for PDFs) draws
// through a Latin-only standard font: Thai text encodes with no error and
// bloats the file the same amount a real mark would, but every glyph is
// silently dropped, so the "watermark" is invisible. This is not new to
// demo mode — nothing in this codebase has ever asked internal/watermark for
// Thai before (the one caller passes a reviewer's ASCII email+timestamp);
// pdfgen/docxgen draw Thai text themselves via gopdf's own TTF registration
// specifically because pdfcpu can't (see main.go's PDFcpu disable-config-dir
// comment). Giving internal/watermark a Thai-capable font is real,
// separate, shared-code work — out of scope for a demo-only safety label.
// "SAMPLE" reads as "not real" in any language a document here is written
// in, which is what this needs to do.
const watermarkText = "SAMPLE — DEMO MODE — NOT REAL DATA"

// DemoDownloadWatermark rewrites every response that looks like a file
// download (has a Content-Disposition header) so it can never pass for a
// real document once it leaves the browser:
//   - PDF bytes get the same tiled text watermark internal/watermark
//     already draws over TA document previews for staff review — reused
//     verbatim, just with a fixed demo string instead of a reviewer's
//     email+timestamp.
//   - Every filename (both the plain and the RFC 5987 UTF-8 parameters —
//     see content_disposition.go's own doc comment for why a real filename
//     needs both) gets a "ตัวอย่าง-" prefix, which is what actually protects
//     formats this package has no watermarker for yet (.docx, .xlsx) — a
//     renamed-but-unmarked file is a real gap, not a solved one; see the
//     doc comment on watermarkAll's docx/xlsx branch below.
//
// Mounted once per slot, wrapping its whole route tree (see handlers.go) —
// not per-route, so it needs no list of which endpoints produce files.
func DemoDownloadWatermark() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if err := c.Next(); err != nil {
			return err
		}
		cd := string(c.Response().Header.Peek(fiber.HeaderContentDisposition))
		if cd == "" {
			return nil // not a file response — the overwhelming majority of requests
		}
		c.Response().Header.Set(fiber.HeaderContentDisposition, prefixDispositionFilenames(cd))

		ct := string(c.Response().Header.ContentType())
		if !strings.HasPrefix(ct, "application/pdf") {
			// docx/xlsx are still only filename-prefixed, not content-marked —
			// OOXML watermarking (editing document.xml/sheet XML inside the
			// zip) is real, separate work internal/watermark does not do
			// today. Tracked as a known gap, not silently skipped: anyone
			// re-zipping or re-saving a demo .docx/.xlsx loses the filename
			// signal, which the PDF path does not depend on.
			return nil
		}
		body := c.Response().Body()
		out, _, err := watermark.Apply(body, "application/pdf", watermarkText)
		if err != nil {
			// A watermark failure must never hand back an UNMARKED real
			// document — better a slightly-broken demo download than a
			// document indistinguishable from production.
			return fiber.NewError(fiber.StatusInternalServerError, "demo: ไม่สามารถประทับลายน้ำเอกสารได้")
		}
		c.Response().SetBodyRaw(out)
		return nil
	}
}

// prefixDispositionFilenames adds "ตัวอย่าง-" to both filename parameters in
// a Content-Disposition header built by handler.contentDisposition — a
// plain string replace rather than a general RFC 6266 parser, since this
// package controls (and tests against) the exact two-parameter shape that
// function always produces: `filename="…"; filename*=UTF-8”…`.
func prefixDispositionFilenames(cd string) string {
	cd = strings.Replace(cd, `filename="`, `filename="ตัวอย่าง-`, 1)
	cd = strings.Replace(cd, `filename*=UTF-8''`, `filename*=UTF-8''`+prefixEscaped, 1)
	return cd
}

// prefixEscaped is "ตัวอย่าง-" percent-encoded the same byte-wise way
// content_disposition.go's rfc5987Escape does, precomputed since the input
// is a fixed constant.
var prefixEscaped = rfc5987EscapeConst("ตัวอย่าง-")

func rfc5987EscapeConst(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
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
