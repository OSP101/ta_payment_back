package service

import (
	"strings"
	"testing"
)

// New uploads are PDF-only (24/07/2026 meeting). The check runs on the file's
// actual leading bytes, because both the Content-Type header and the filename
// extension are fully client-controlled — renaming photo.jpg to photo.pdf must
// not get past the door.

func TestSniffAllowedDoc_AcceptsOnlyPDF(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want bool
	}{
		{"pdf magic", []byte("%PDF-1.7\n%..."), true},
		{"pdf minimal", []byte("%PDF"), true},

		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, false},
		{"png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, false},
		{"gif", []byte("GIF89a"), false},
		{"zip / office file", []byte{0x50, 0x4B, 0x03, 0x04}, false},
		{"plain text", []byte("hello, this is not a pdf"), false},
		{"empty", nil, false},
		{"pdf marker not at the start", []byte("  %PDF-1.7"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sniffAllowedDoc(c.head); got != c.want {
				t.Errorf("sniffAllowedDoc(%q) = %v, want %v", c.head, got, c.want)
			}
		})
	}
}

func TestAcceptedDocMIMEs_IsPDFOnly(t *testing.T) {
	if !acceptedDocMIMEs["application/pdf"] {
		t.Error("PDF must be accepted")
	}
	for _, m := range []string{"image/jpeg", "image/jpg", "image/png", "image/heic", "text/plain"} {
		if acceptedDocMIMEs[m] {
			t.Errorf("%s must no longer be accepted for new uploads", m)
		}
	}
	if len(acceptedDocMIMEs) != 1 {
		t.Errorf("expected exactly one accepted MIME, got %d", len(acceptedDocMIMEs))
	}
}

// The per-file ceiling is quoted to TAs in three separate messages and on two
// file pickers, so it is pinned here: a silent change to the constant would leave
// the UI promising one number while the server enforced another.
func TestMaxDocBytesIs10MB(t *testing.T) {
	if maxDocBytes != 10*1024*1024 {
		t.Errorf("maxDocBytes = %d, want %d", maxDocBytes, 10*1024*1024)
	}
}

// The refusal must quote the real limit. Hard-coding "2 MB" in the message is
// exactly how the constant and the text drifted apart before.
func TestTooLargeMessageQuotesTheActualLimit(t *testing.T) {
	msg := errDocTooLarge().Error()
	if !strings.Contains(msg, maxDocMBLabel+" MB") {
		t.Errorf("refusal %q does not quote the %s MB limit", msg, maxDocMBLabel)
	}
}

// Tightening the door must not orphan what is already inside. Plenty of stored
// rows are JPEG/PNG from before the rule; the review screen still previews them
// by extension, so that mapping has to keep working.
func TestLegacyImageDocsRemainViewable(t *testing.T) {
	for _, name := range []string{"idcard.jpg", "book.JPEG", "scan.png", "SCAN.PNG"} {
		ext := filenameExt(name)
		switch ext {
		case "jpg", "jpeg", "png":
			// recognised — the UI renders these with <img>
		default:
			t.Errorf("filenameExt(%q) = %q, which the viewer would not treat as an image", name, ext)
		}
	}
	if got := filenameExt("form.pdf"); got != "pdf" {
		t.Errorf("filenameExt(form.pdf) = %q, want pdf", got)
	}
}
