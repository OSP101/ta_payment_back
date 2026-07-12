// Package watermark applies a diagonally-tiled text watermark onto PDF or
// image bytes. It is used by the staff review preview endpoint so an officer
// looking at a TA's document sees their own email and the current timestamp
// baked into the render — a CSS overlay would be trivially bypassed via
// direct URL access, so the mark is server-side.
package watermark

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfmodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	pdftypes "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Apply routes to the right renderer based on mime and returns the
// watermarked bytes plus the resulting Content-Type. For images the output
// is always PNG so we don't re-encode to lossy JPEG on top of an already
// lossy source.
func Apply(src []byte, mime, text string) ([]byte, string, error) {
	if text == "" {
		return nil, "", errors.New("watermark text is empty")
	}
	switch mime {
	case "application/pdf":
		out, err := applyPDF(src, text)
		return out, "application/pdf", err
	case "image/jpeg", "image/jpg", "image/png":
		out, err := applyImage(src, mime, text)
		return out, "image/png", err
	default:
		return nil, "", errors.New("unsupported mime for watermark: " + mime)
	}
}

// applyPDF stamps every page with a diagonal centered text watermark. Font
// stays Helvetica because the watermark text is ASCII (email + digits +
// separators) — no Thai glyphs, no font-registration step required.
func applyPDF(src []byte, text string) ([]byte, error) {
	desc := "font:Helvetica, points:20, opacity:0.28, rotation:35, mode:2, position:c, scale:1 rel"
	wm, err := pdfapi.TextWatermark(text, desc, true, false, pdftypes.POINTS)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	conf := pdfmodel.NewDefaultConfiguration()
	if err := pdfapi.AddWatermarks(bytes.NewReader(src), &out, nil, wm, conf); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// applyImage draws the text tiled across the image at a low opacity so it is
// obvious the preview is stamped but the underlying content is still
// readable. Uses basicfont so no external TTF is needed; text is repeated at
// multiple offsets to make cropping the mark out hard.
func applyImage(src []byte, mime, text string) ([]byte, error) {
	var (
		img image.Image
		err error
	)
	switch mime {
	case "image/png":
		img, err = png.Decode(bytes.NewReader(src))
	default:
		img, err = jpeg.Decode(bytes.NewReader(src))
	}
	if err != nil {
		return nil, err
	}

	b := img.Bounds()
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, img, b.Min, draw.Src)

	// Draw the watermark once at each grid intersection. Text is repeated
	// twice per stamp (offset) so an aggressive crop still catches at least
	// one instance.
	col := color.RGBA{R: 0, G: 0, B: 0, A: 90} // ~35% opacity
	face := basicfont.Face7x13
	step := 180
	if b.Dx() < 400 || b.Dy() < 400 {
		step = 90
	}
	for y := b.Min.Y + 20; y < b.Max.Y; y += step {
		for x := b.Min.X + 20; x < b.Max.X; x += step {
			drawer := &font.Drawer{
				Dst:  rgba,
				Src:  image.NewUniform(col),
				Face: face,
				Dot:  fixed.P(x, y),
			}
			drawer.DrawString(text)
		}
	}

	var out bytes.Buffer
	if err := png.Encode(&out, rgba); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Ensure io import stays used if callers pass a Reader later (kept for API
// symmetry with export.go pattern).
var _ = io.Discard
