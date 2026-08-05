// Package pdfgen fills the KKU "แบบแจ้งข้อมูลเจ้าหนี้บุคลากร" PDF by overlaying
// data from the TA profile onto the shipped blank template. Every field is
// drawn at a hard-coded point coordinate — the template is treated as a fixed
// visual asset, and the overlay must be calibrated whenever the finance office
// re-issues it. To recalibrate, request the preview endpoint with grid=1 and
// read positions off the debug grid.
//
// We use signintech/gopdf: it wraps gofpdi (for importing an existing PDF page
// as a background) and can register a TTF font for Thai text. The template is
// A4 (595.28 × 841.89 pt); coordinates are in points from the top-left. The
// PDF coordinate system has origin at bottom-left, but gopdf's SetXY/Cell use
// a top-left origin, so all coordinates in this file are top-left-based.
package pdfgen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pdfcpu "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfcpuModel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/signintech/gopdf"
)

// gofpdi (used by gopdf.ImportPage) chokes on cross-reference streams with
// non-trivial /DecodeParms, which the shipped template happens to use. We
// preprocess the template once with pdfcpu — which understands modern PDFs —
// into a temp file that gofpdi is happy to parse. The result is cached per
// source path so we don't rewrite it on every render.
var (
	normalizedMu sync.Mutex
	normalized   = map[string]string{}
)

func normalizedTemplate(src string) (string, error) {
	normalizedMu.Lock()
	defer normalizedMu.Unlock()
	if cached, ok := normalized[src]; ok {
		if _, err := os.Stat(cached); err == nil {
			return cached, nil
		}
	}
	tmp := filepath.Join(os.TempDir(), "ta-creditor-tpl.pdf")
	// gofpdi only reads plain PDF 1.4-style structure: no cross-reference
	// streams and no object streams. Force pdfcpu to emit that shape.
	conf := pdfcpuModel.NewDefaultConfiguration()
	conf.WriteObjectStream = false
	conf.WriteXRefStream = false
	if err := pdfcpu.OptimizeFile(src, tmp, conf); err != nil {
		return "", fmt.Errorf("normalize template: %w", err)
	}
	normalized[src] = tmp
	return tmp, nil
}

// CreditorData is the payload written onto the form.
type CreditorData struct {
	Prefix       string // exactly one of "นาย" / "นาง" / "นางสาว"
	FullName     string
	NationalID   string // 13 digits; non-digits are stripped before placement
	Phone        string
	Email        string
	AccountName  string
	BankName     string
	BranchCode   string
	Branch       string
	AccountNo    string
	SignaturePNG []byte
	// Date components. Default to today in Thai calendar if all three are blank.
	Day   string
	Month string
	Year  string
}

// CreditorInput controls a single render.
type CreditorInput struct {
	TemplatePath string // path to the blank PDF (single page, A4)
	FontDir      string // directory containing Sarabun-Regular.ttf / Sarabun-Bold.ttf
	Debug        bool   // draw a coordinate grid for calibration
	Data         CreditorData
}

const (
	pageW = 595.28 // A4 width in points
	pageH = 841.89 // A4 height in points
)

// coords holds hard-coded overlay positions in points from the top-left of the
// A4 page. Values are first-pass estimates that must be tuned against the real
// PDF preview — the debug grid overlay (Debug=true) is how we do that.
type coords struct {
	// Row 2 — "(นาย/นาง/นางสาว)" circle + full name.
	prefixNaiCX     float64
	prefixNangCX    float64
	prefixNangsaoCX float64
	prefixCY        float64
	prefixRX        float64
	prefixRY        float64
	nameX, nameY    float64

	// Row 5 — national ID as 13 digit cells.
	nidX0    float64
	nidY     float64
	nidCellW float64

	// Row 6 — phone / email.
	phoneX, phoneY float64
	emailX, emailY float64

	// Row 7 — bank block. Account digits reuse nidX0/nidCellW because the
	// template draws the same 13-cell box grid on the เลขที่บัญชี row.
	accountNameX, accountNameY float64
	bankNameX, bankNameY       float64
	branchCodeX, branchCodeY   float64
	branchX, branchY           float64
	accountNoY                 float64

	// Row 8 — check marks.
	kkuStudentX, kkuStudentY float64
	newDataX, newDataY       float64

	// Row 9 — signature image bounding box + printed name in parens below it.
	sigX, sigY, sigW, sigH     float64
	printedNameX, printedNameY float64

	// Row 10 — date.
	dayX, dayY     float64
	monthX, monthY float64
	yearX, yearY   float64
}

// c is the calibrated set of positions. Values are in points from the top-left
// of the A4 page (595.32 × 841.92). Iterate against the /me/creditor-form.pdf
// preview with ?grid=1 to fine-tune — the grid draws every 10 pt with axis
// labels so you can read the target position off a real preview.
//
// Note: gopdf.Cell writes text with a ~12 pt baseline offset (Sarabun 11pt),
// so gopdf_Y ≈ target_visible_Y - 12 for text fields. Line() and Oval() have
// no such offset — draw them at their target visible Y directly.
//
// Row baselines in the template (all visible Y from top):
//
//	ชื่อ-สกุล          =  95.9
//	ประเภท              = 120.4
//	ลักษณะการแจ้ง       = 145.0
//	NID label (2 lines) = 174.7 / 193.4  →  digit boxes centred ~184
//	โทรศัพท์ / E-Mail    = 217.9
//	ข้อมูลธนาคาร (header)= 242.4
//	ชื่อบัญชี            = 263.8
//	ชื่อธนาคาร           = 306.6
//	รหัสสาขา / ชื่อสาขา   = 330.6
//	เลขที่บัญชี         = 360.6
//	ลงชื่อ               = 417.9
//	วันที่               = 479.1
var c = coords{
	// Prefix circles live inside "(นาย/นาง/นางสาว)" on the ชื่อ-สกุล row.
	// Row baseline is 95.9; the glyphs' vertical centre sits ~4 pt above it.
	prefixNaiCX:     190,
	prefixNangCX:    213,
	prefixNangsaoCX: 243,
	prefixCY:        92, // Oval — no baseline offset
	prefixRX:        13,
	prefixRY:        9,
	nameX:           275, nameY: 83, // Cell — → visible ~95

	// NID row — 13 digit boxes drawn as XObjects at visible Y=159..193 with
	// centres at X = 185, 203, 221, …, 401 (18 pt spacing). Digit glyph is
	// ~6 pt wide so nidX0 = box_center - 3 = 182; centre baseline in box at
	// visible Y=178 → gopdf Y = 178 - 12 baseline offset = 166.
	nidX0:    182,
	nidY:     166,
	nidCellW: 18,

	// Phone / E-Mail row — baseline 217.9. Labels occupy X=97..155 ("โทรศัพท์
	// ติดต่อ:") and X=347..381 ("E-Mail:"), so values start after each label.
	phoneX: 200, phoneY: 206,
	emailX: 385, emailY: 206,

	// Bank block.
	accountNameX: 210, accountNameY: 252, // baseline 263.8
	bankNameX: 175, bankNameY: 295, // 306.6
	branchCodeX: 175, branchCodeY: 319, // 330.6 (same row as ชื่อสาขา)
	branchX: 320, branchY: 319,
	// Account digits go into the same 13-cell box grid as the NID row
	// (identical X layout on the template); Y targets the boxes at
	// visible 341.7..375.5 (centre 358.6, digit baseline ≈361).
	accountNoY: 349,

	// Check marks — Line() so no baseline offset. Checkboxes are XObjects on
	// the template with these centres (extracted from the content stream):
	//   ประเภท (Y_center=117):     บุคลากร X=186  กรรมการ X=255  นักศึกษา X=447
	//   ลักษณะการแจ้ง (Y_center=142): เพิ่มใหม่ X=186  แก้ไข X=259  เปลี่ยนบัญชี X=372
	// Check-mark glyph spans (x, y-3) → (x+12, y+8) so x = box_center - 6,
	// y = box_center_Y - 2 places it centred inside the ☐ frame.
	kkuStudentX: 441, kkuStudentY: 115, // 3rd checkbox on ประเภท row
	newDataX: 180, newDataY: 140, // 1st checkbox on ลักษณะการแจ้ง row

	// Signature block (extracted): "ลงชื่อ" label at X=305.7 with its dotted
	// line spanning X=329..455, baseline 441.9. The image sits on the dots.
	sigX: 337, sigY: 414, sigW: 110, sigH: 28,

	// Printed name in the "(………)" row under the signature — parens span
	// X=319..452 at baseline 460.5; centre the name inside them.
	printedNameX: 335, printedNameY: 448,

	// Date line — a single text run at baseline 503.1 starting X=292:
	// "วันที่ ...(day)... เดือน ...(month)... พ.ศ. ...(year)..."
	dayX: 320, dayY: 491,
	monthX: 360, monthY: 491,
	yearX: 442, yearY: 491,
}

// FillCreditor renders the filled creditor-form PDF as bytes.
func FillCreditor(in CreditorInput) ([]byte, error) {
	d := in.Data
	if d.Day == "" && d.Month == "" && d.Year == "" {
		now := time.Now()
		d.Day = fmt.Sprintf("%d", now.Day())
		d.Month = thaiMonth(int(now.Month()))
		d.Year = fmt.Sprintf("%d", now.Year()+543)
	}

	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})

	if err := pdf.AddTTFFont("sarabun", in.FontDir+"/Sarabun-Regular.ttf"); err != nil {
		return nil, fmt.Errorf("register sarabun regular: %w", err)
	}
	if err := pdf.AddTTFFont("sarabunb", in.FontDir+"/Sarabun-Bold.ttf"); err != nil {
		return nil, fmt.Errorf("register sarabun bold: %w", err)
	}

	pdf.AddPage()

	// Import the blank template PDF and stamp it as the background. Route
	// through the pdfcpu-normalized copy so gofpdi can parse it.
	tplPath, err := normalizedTemplate(in.TemplatePath)
	if err != nil {
		return nil, err
	}
	tpl := pdf.ImportPage(tplPath, 1, "/MediaBox")
	pdf.UseImportedTemplate(tpl, 0, 0, pageW, pageH)

	if err := pdf.SetFont("sarabun", "", 11); err != nil {
		return nil, err
	}
	pdf.SetLineWidth(0.8)
	pdf.SetStrokeColor(0, 0, 0)
	pdf.SetFillColor(0, 0, 0)
	pdf.SetTextColor(0, 0, 0)

	// Prefix circle.
	switch d.Prefix {
	case "นาย":
		drawOval(&pdf, c.prefixNaiCX, c.prefixCY, c.prefixRX, c.prefixRY)
	case "นาง":
		drawOval(&pdf, c.prefixNangCX, c.prefixCY, c.prefixRX, c.prefixRY)
	case "นางสาว":
		drawOval(&pdf, c.prefixNangsaoCX, c.prefixCY, c.prefixRX, c.prefixRY)
	}

	setText(&pdf, c.nameX, c.nameY, d.FullName)

	// National ID: 13 digits spread across the boxes.
	nid := onlyDigits(d.NationalID)
	for i, r := range nid {
		if i >= 13 {
			break
		}
		setText(&pdf, c.nidX0+float64(i)*c.nidCellW, c.nidY, string(r))
	}

	setText(&pdf, c.phoneX, c.phoneY, d.Phone)
	setText(&pdf, c.emailX, c.emailY, d.Email)
	setText(&pdf, c.accountNameX, c.accountNameY, d.AccountName)
	// The label on the form already reads "ชื่อธนาคาร", so strip a leading
	// "ธนาคาร" from the stored name ("ธนาคารไทยพาณิชย์" → "ไทยพาณิชย์").
	setText(&pdf, c.bankNameX, c.bankNameY,
		strings.TrimSpace(strings.TrimPrefix(d.BankName, "ธนาคาร")))
	setText(&pdf, c.branchCodeX, c.branchCodeY, d.BranchCode)
	setText(&pdf, c.branchX, c.branchY, d.Branch)

	// Account number: one digit per box, same 13-cell grid as the NID row.
	acct := onlyDigits(d.AccountNo)
	for i, r := range acct {
		if i >= 13 {
			break
		}
		setText(&pdf, c.nidX0+float64(i)*c.nidCellW, c.accountNoY, string(r))
	}

	// Bump the stroke width just for the checkmarks so the tick reads clearly
	// on the small checkboxes on the form. Reset immediately after.
	pdf.SetLineWidth(1.4)
	drawCheck(&pdf, c.kkuStudentX, c.kkuStudentY)
	drawCheck(&pdf, c.newDataX, c.newDataY)
	pdf.SetLineWidth(0.8)

	if len(d.SignaturePNG) > 0 {
		if err := placePNG(&pdf, d.SignaturePNG, c.sigX, c.sigY, c.sigW, c.sigH); err != nil {
			return nil, fmt.Errorf("place signature: %w", err)
		}
	}

	// Printed full name (with prefix, Thai style — no space after prefix)
	// inside the "(………)" parens under the signature line.
	if d.FullName != "" {
		setText(&pdf, c.printedNameX, c.printedNameY, d.Prefix+d.FullName)
	}

	setText(&pdf, c.dayX, c.dayY, d.Day)
	setText(&pdf, c.monthX, c.monthY, d.Month)
	setText(&pdf, c.yearX, c.yearY, d.Year)

	if in.Debug {
		drawGrid(&pdf)
	}

	var buf bytes.Buffer
	if _, err := pdf.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func setText(pdf *gopdf.GoPdf, x, y float64, s string) {
	if s == "" {
		return
	}
	pdf.SetXY(x, y)
	_ = pdf.Cell(nil, s)
}

func drawOval(pdf *gopdf.GoPdf, cx, cy, rx, ry float64) {
	pdf.Oval(cx-rx, cy-ry, cx+rx, cy+ry)
}

// drawCheck draws a "✓" mark using two line strokes. The mark's inflection
// point sits at (x+4, y+8); the whole glyph fits roughly in a 14x14 box.
// Sized to fill a KKU-form checkbox (~10-14pt square) so the tick is unambiguous.
func drawCheck(pdf *gopdf.GoPdf, x, y float64) {
	pdf.Line(x, y+4, x+4, y+8)
	pdf.Line(x+4, y+8, x+12, y-3)
}

func placePNG(pdf *gopdf.GoPdf, png []byte, x, y, w, h float64) error {
	holder, err := gopdf.ImageHolderByBytes(png)
	if err != nil {
		return err
	}
	return pdf.ImageByHolder(holder, x, y, &gopdf.Rect{W: w, H: h})
}

// drawGrid overlays a coordinate grid so field positions can be measured off a
// preview. Minor gridlines every 10 pt (light red); major gridlines every 50 pt
// (darker red with axis labels). Debug-only; the /me/creditor-form.pdf route
// enables it when the requester passes ?grid=1.
func drawGrid(pdf *gopdf.GoPdf) {
	_ = pdf.SetFont("sarabun", "", 6)
	// Minor grid: every 10 pt, light red, thin.
	pdf.SetLineWidth(0.2)
	pdf.SetStrokeColor(230, 180, 180)
	for x := 0.0; x < pageW; x += 10 {
		pdf.Line(x, 0, x, pageH)
	}
	for y := 0.0; y < pageH; y += 10 {
		pdf.Line(0, y, pageW, y)
	}
	// Major grid: every 50 pt, darker red, with axis labels.
	pdf.SetLineWidth(0.5)
	pdf.SetStrokeColor(200, 40, 40)
	pdf.SetTextColor(150, 30, 30)
	for x := 0.0; x < pageW; x += 50 {
		pdf.Line(x, 0, x, pageH)
		pdf.SetXY(x+1, 8)
		_ = pdf.Cell(nil, fmt.Sprintf("%.0f", x))
	}
	for y := 0.0; y < pageH; y += 50 {
		pdf.Line(0, y, pageW, y)
		pdf.SetXY(2, y+1)
		_ = pdf.Cell(nil, fmt.Sprintf("%.0f", y))
	}
	pdf.SetLineWidth(0.8)
	pdf.SetStrokeColor(0, 0, 0)
	pdf.SetTextColor(0, 0, 0)
}

func onlyDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func thaiMonth(m int) string {
	names := []string{"", "มกราคม", "กุมภาพันธ์", "มีนาคม", "เมษายน", "พฤษภาคม", "มิถุนายน",
		"กรกฎาคม", "สิงหาคม", "กันยายน", "ตุลาคม", "พฤศจิกายน", "ธันวาคม"}
	if m < 1 || m > 12 {
		return ""
	}
	return names[m]
}
