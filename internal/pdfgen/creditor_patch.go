package pdfgen

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	pdfcpu "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfcpuModel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/signintech/gopdf"
)

// CreditorPatchInput rewrites the identity fields of a creditor form the TA
// already signed. The bank details and the signature are NOT stored anywhere
// (migration 0047) — they exist only inside that PDF — so a correction cannot
// re-render the form from data. Instead the TA's own PDF is the background, and
// only the regions holding the corrected fields are repainted: each is covered
// with the BLANK template clipped to that region (restoring its printed rules,
// dots and labels), then written again.
type CreditorPatchInput struct {
	SourcePDF    []byte // the TA's stored, signed creditor form
	TemplatePath string // the blank form, for repainting regions
	FontDir      string
	Prefix       string
	FullName     string
	NationalID   string
}

// ErrNotGeneratedCreditorForm: the stored file is not a one-page A4 form this
// system rendered, so there is nothing to patch.
var ErrNotGeneratedCreditorForm = errors.New("stored creditor form is not a generated one-page A4 form")

// patchRegion is a rectangle in top-left points.
type patchRegion struct{ x, y, w, h float64 }

var (
	// The prefix row "(นาย/นาง/นางสาว)" with room for the ring's stroke.
	regionPrefix = patchRegion{180, 83.5, 86, 22}
	// The name's dotted run, from just past the prefix row to the margin.
	regionName = patchRegion{268, 84, 280, 18}
	// The 13 ID boxes: interiors centred on y=174.2, 16.1 pt tall. The region
	// spans whole boxes, rules included — the blank template redraws them.
	regionNID = patchRegion{176, 164.5, 13*20.99 + 4, 19.5}
	// Between the brackets of "(………)" under the signature. The signature image
	// itself sits above baseline 448.1, clear of this.
	regionPrintedName = patchRegion{335.8, 455, 136.6, 18}
)

// PatchCreditor returns the corrected form.
func PatchCreditor(in CreditorPatchInput) ([]byte, error) {
	dir, err := os.MkdirTemp("", "ta-creditor-patch-")
	if err != nil {
		return nil, err
	}
	// The source is a signed form full of PII: never leave it in /tmp.
	defer os.RemoveAll(dir)

	raw := filepath.Join(dir, "src.pdf")
	if err := os.WriteFile(raw, in.SourcePDF, 0o600); err != nil {
		return nil, err
	}
	src := filepath.Join(dir, "src-norm.pdf")
	conf := pdfcpuModel.NewDefaultConfiguration()
	conf.WriteObjectStream = false
	conf.WriteXRefStream = false
	if err := pdfcpu.OptimizeFile(raw, src, conf); err != nil {
		return nil, fmt.Errorf("read stored creditor form: %w", err)
	}
	// Only a form this system generated can be patched — the field positions
	// are the template's. A file of another shape (a hand-made placeholder, a
	// scan) would come back as a blank page with a few fields floating on it,
	// so refuse it instead.
	dims, err := pdfcpu.PageDimsFile(src)
	if err != nil || len(dims) != 1 || math.Abs(dims[0].Width-pageW) > 2 || math.Abs(dims[0].Height-pageH) > 2 {
		return nil, ErrNotGeneratedCreditorForm
	}
	blank, err := normalizedTemplate(in.TemplatePath)
	if err != nil {
		return nil, err
	}

	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	if err := pdf.AddTTFFont("sarabun", in.FontDir+"/Sarabun-Regular.ttf"); err != nil {
		return nil, fmt.Errorf("register sarabun regular: %w", err)
	}
	pdf.AddPage()
	signed := pdf.ImportPage(src, 1, "/MediaBox")
	pdf.UseImportedTemplate(signed, 0, 0, pageW, pageH)
	tpl := pdf.ImportPage(blank, 1, "/MediaBox")

	repaint := func(r patchRegion) {
		pdf.SaveGraphicsState()
		pdf.ClipPolygon([]gopdf.Point{{X: r.x, Y: r.y}, {X: r.x + r.w, Y: r.y}, {X: r.x + r.w, Y: r.y + r.h}, {X: r.x, Y: r.y + r.h}})
		pdf.SetFillColor(255, 255, 255)
		pdf.RectFromUpperLeftWithStyle(r.x, r.y, r.w, r.h, "F")
		pdf.UseImportedTemplate(tpl, 0, 0, pageW, pageH)
		pdf.RestoreGraphicsState()
	}
	for _, r := range []patchRegion{regionPrefix, regionName, regionNID, regionPrintedName} {
		repaint(r)
	}

	if err := pdf.SetFont("sarabun", "", bodyFont); err != nil {
		return nil, err
	}
	pdf.SetLineWidth(0.8)
	pdf.SetStrokeColor(0, 0, 0)
	pdf.SetFillColor(0, 0, 0)
	pdf.SetTextColor(0, 0, 0)
	if ring, ok := c.prefixes[in.Prefix]; ok {
		drawOval(&pdf, ring.cx, c.prefixCY, ring.rx, c.prefixRY)
	}
	setTextInField(&pdf, c.name, in.FullName, alignLeft)
	fillGrid(&pdf, c.nid, idChars(in.NationalID))
	if in.FullName != "" {
		setTextInField(&pdf, c.printedName, printedName(in.Prefix, in.FullName), alignCenter)
	}

	var buf bytes.Buffer
	if _, err := pdf.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
