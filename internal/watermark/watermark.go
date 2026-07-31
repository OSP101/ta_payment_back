// Package watermark applies a diagonally-tiled text watermark onto PDF or
// image bytes. It is used by the staff review preview endpoint so an officer
// looking at a TA's document sees their own email and the current timestamp
// baked into the render — a CSS overlay would be trivially bypassed via
// direct URL access, so the mark is server-side.
package watermark

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"strings"

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

// Watermark geometry.
//
// The mark has to blanket the page: a single line, or a tile that only covers
// part of it, can be cropped away around whatever field was worth stealing.
//
// The subtlety that produced two wrong attempts before this one: pdfcpu
// preserves the watermark's aspect ratio and shrinks it to fit inside
// `scale × page`, so whichever dimension binds first leaves the other short.
// A tile wider than it is tall (relative to A4) fits by width and leaves the top
// and bottom bare — which is exactly the "กองไปอยู่ตรงกลาง" complaint. Rotating
// it makes this worse, not better: the rotated bounding box is what gets fitted,
// so the text ends up as a band across the middle with all four corners empty,
// and pdfcpu rejects any relative scale above 1.0 that would let it overflow.
//
// So the tile is built to A4's proportions instead of guessed at, and left
// unrotated so its box maps straight onto the page. Then `scale:1 rel` fills the
// page by construction rather than by luck.
const (
	wmPoints   = 8
	wmLeading  = 1.2 * wmPoints // pdfcpu's line spacing for multi-line text
	wmCharWide = 0.5 * wmPoints // Helvetica's average advance, near enough
	wmCols     = 3
	// A4 portrait, matching the creditor form (595.32 × 841.92 pt).
	wmPageAspect = 841.92 / 595.32
)

// tiledText repeats the identity string into a block shaped like the page.
//
// Row count is derived, not hard-coded, so the block keeps covering the page
// when the text length changes — an officer with a longer email would otherwise
// silently shift the aspect ratio and bring the bare bands back.
func tiledText(text string) string {
	cell := text + "    "
	lineWidth := float64(wmCols*len(cell)) * wmCharWide
	rows := int(lineWidth * wmPageAspect / wmLeading)
	if rows < 8 {
		rows = 8
	}

	var b strings.Builder
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteByte('\n')
		}
		// Stagger alternate rows by half a cell: aligned columns leave vertical
		// gutters wide enough to read a whole field through.
		if r%2 == 1 {
			b.WriteString(strings.Repeat(" ", len(cell)/2))
		}
		for c := 0; c < wmCols; c++ {
			b.WriteString(cell)
		}
	}
	return b.String()
}

// applyPDF stamps every page with a dense tiled text watermark.
//
// What this does NOT do — worth stating because it is the obvious thing to
// assume: it cannot stop a screenshot. Blacking out screen capture (as Netflix
// does) is DRM enforced by the OS and GPU, unavailable to any web page. What a
// dense mark buys is different and still useful: every capture carries the
// identity of the officer who opened it, so a leak is attributable and a forged
// reuse is obvious.
func applyPDF(src []byte, text string) ([]byte, error) {
	// Faint enough that the form underneath stays readable, repeated often
	// enough that no crop escapes it — legibility is governed by opacity here,
	// not by density. mode:2 is fill+stroke, which keeps thin glyphs visible
	// over dark scan areas.
	desc := fmt.Sprintf(
		"font:Helvetica, points:%d, opacity:0.17, rotation:0, mode:2, position:c, scale:1 rel",
		wmPoints)
	wm, err := pdfapi.TextWatermark(tiledText(text), desc, true, false, pdftypes.POINTS)
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

	// Dense lattice, matching applyPDF's reasoning: a sparse mark is croppable,
	// so the grid is tight enough that no readable region is unmarked. Opacity
	// drops as density rises — the total ink is what decides legibility, not the
	// number of stamps.
	col := color.RGBA{R: 0, G: 0, B: 0, A: 46} // ~18%
	face := basicfont.Face7x13
	// Roughly one stamp per text width, so stamps nearly touch horizontally.
	stepX := 7*len(text) + 24
	stepY := 26
	if b.Dx() < 400 || b.Dy() < 400 {
		stepY = 18
	}
	row := 0
	for y := b.Min.Y + 14; y < b.Max.Y+stepY; y += stepY {
		// Stagger alternate rows so the gaps between stamps never line up into
		// a clear vertical channel.
		shift := 0
		if row%2 == 1 {
			shift = stepX / 2
		}
		for x := b.Min.X - shift; x < b.Max.X; x += stepX {
			drawer := &font.Drawer{
				Dst:  rgba,
				Src:  image.NewUniform(col),
				Face: face,
				Dot:  fixed.P(x, y),
			}
			drawer.DrawString(text)
		}
		row++
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
