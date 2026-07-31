package handler

import (
	"bytes"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func pngBytes(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return b.Bytes()
}

func TestNormalizeAvatarReencodesToJPEG(t *testing.T) {
	out, err := normalizeAvatar(pngBytes(t, 300, 300, color.NRGBA{R: 10, G: 200, B: 40, A: 255}))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not a JPEG: %v", err)
	}
	if got := img.Bounds().Dx(); got != 300 {
		t.Fatalf("width = %d, want the source 300 (no upscaling)", got)
	}
}

func TestNormalizeAvatarDownscalesToStoredEdge(t *testing.T) {
	// A 1600×800 source: the long side is capped, the aspect ratio is kept.
	out, err := normalizeAvatar(pngBytes(t, 1600, 800, color.NRGBA{R: 1, G: 2, B: 3, A: 255}))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if img.Bounds().Dx() != avatarStoredEdge || img.Bounds().Dy() != avatarStoredEdge/2 {
		t.Fatalf("bounds = %v, want %dx%d", img.Bounds(), avatarStoredEdge, avatarStoredEdge/2)
	}
}

// Transparency must land on white, not black — the usual giveaway that a PNG
// went through a JPEG encoder untouched.
func TestNormalizeAvatarFlattensTransparencyToWhite(t *testing.T) {
	out, err := normalizeAvatar(pngBytes(t, 64, 64, color.NRGBA{A: 0}))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r, g, b, _ := img.At(32, 32).RGBA()
	if r < 0xf000 || g < 0xf000 || b < 0xf000 {
		t.Fatalf("centre pixel = (%d,%d,%d), want near-white", r, g, b)
	}
}

func TestNormalizeAvatarRejectsNonImage(t *testing.T) {
	_, err := normalizeAvatar([]byte("<?php system($_GET['c']); ?>"))
	if err == nil {
		t.Fatal("expected a rejection")
	}
	fe, ok := err.(*fiber.Error)
	if !ok || fe.Code != fiber.StatusUnsupportedMediaType {
		t.Fatalf("err = %v, want 415", err)
	}
}

// A tiny PNG header can declare an enormous canvas; the guard must fire before
// any pixels are allocated.
func TestNormalizeAvatarRejectsDecompressionBomb(t *testing.T) {
	hdr := pngBytes(t, 1, 1, color.Black)
	// Rewrite the IHDR width/height to 30000×30000 — a 1 KB file that claims a
	// 3.6 GB canvas. The chunk CRC has to be fixed up too, or the decoder
	// rejects it as corrupt and the test proves nothing about the guard.
	big := append([]byte(nil), hdr...)
	putU32(big[16:], 30000) // IHDR width
	putU32(big[20:], 30000) // IHDR height
	putU32(big[29:], crc32.ChecksumIEEE(big[12:29]))
	_, err := normalizeAvatar(big)
	if err == nil {
		t.Fatal("expected a rejection")
	}
	fe, ok := err.(*fiber.Error)
	if !ok || fe.Code != fiber.StatusRequestEntityTooLarge {
		t.Fatalf("err = %v, want 413", err)
	}
}

func putU32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}
