package watermark

// Visual-inspection harness. Skipped unless WM_RENDER is set.
//
// Watermark coverage cannot be asserted from Go — whether the mark reaches the
// page edges is a question about rendered geometry, and pdfcpu will happily
// produce a valid PDF whose watermark sits in a band across the middle. This
// writes the real creditor-form template with the current settings so the result
// can be opened and looked at:
//
//	WM_RENDER=1 WM_OUT=/tmp/wm.pdf go test ./internal/watermark/ -run TestManualRender

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestManualRender(t *testing.T) {
	if os.Getenv("WM_RENDER") == "" {
		t.Skip("set WM_RENDER=1 to write a sample")
	}
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	src, err := os.ReadFile(filepath.Join(root, "assets", "creditor_form_template.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := Apply(src, "application/pdf", "supphitan.p@kkumail.com | 2026-07-30 12:57")
	if err != nil {
		t.Fatal(err)
	}
	dst := os.Getenv("WM_OUT")
	if dst == "" {
		dst = filepath.Join(os.TempDir(), "wm-sample.pdf")
	}
	if err := os.WriteFile(dst, out, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d bytes)", dst, len(out))
}
