package demo

import (
	"strings"
	"testing"
)

// DEMO-03: the demo watermark used to put raw Thai bytes ("ตัวอย่าง-") into
// the `filename=` parameter, which HTTP requires to be ISO-8859-1 — exactly
// the mistake handler.asciiFilenameFallback exists to prevent on the
// production path. A client that reads only `filename=` (curl -OJ, a simple
// parser, an email gateway) got a mangled name instead of a demo signal.
// This pins that `filename=` is now pure ASCII and `filename*=` still
// carries the (percent-encoded) Thai prefix.
func TestPrefixDispositionFilenames_PlainParamStaysASCII(t *testing.T) {
	// Shape handler.contentDisposition actually produces for a Thai filename:
	// filename= is already the ASCII fallback (asciiFilenameFallback), never
	// raw Thai — this middleware's job is to prefix that ASCII value without
	// reintroducing non-ASCII bytes into it.
	cd := `attachment; filename="download.pdf"; filename*=UTF-8''%E0%B8%84%E0%B8%B3%E0%B8%AA%E0%B8%B1%E0%B9%88%E0%B8%87.pdf`
	got := prefixDispositionFilenames(cd)

	start := strings.Index(got, `filename="`) + len(`filename="`)
	end := strings.Index(got[start:], `"`) + start
	plain := got[start:end]
	for i := 0; i < len(plain); i++ {
		if plain[i] >= 0x80 {
			t.Fatalf("filename= must be pure ASCII, got byte 0x%X in %q", plain[i], plain)
		}
	}
	if !strings.HasPrefix(plain, "SAMPLE-") {
		t.Fatalf(`filename= = %q, want it to start with "SAMPLE-"`, plain)
	}

	if !strings.Contains(got, `filename*=UTF-8''%E0%B8%95%E0%B8%B1%E0%B8%A7`) {
		t.Fatalf("filename*= should still carry the percent-encoded Thai prefix (ตัวอย่าง-), got: %s", got)
	}
}
