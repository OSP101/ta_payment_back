// Package pdfgen fills the KKU "แบบแจ้งข้อมูลเจ้าหนี้บุคลากร" PDF by overlaying
// data from the TA profile onto the shipped blank template. Every field is
// drawn at a hard-coded point coordinate — the template is treated as a fixed
// visual asset, and the overlay must be calibrated whenever the finance office
// re-issues it. See the `c` var below for how the current numbers were
// measured; the preview endpoint's grid=1 debug overlay is the quick way to
// sanity-check a change against a real render.
//
// We use signintech/gopdf: it wraps gofpdi (for importing an existing PDF page
// as a background) and can register a TTF font for Thai text. Coordinates are
// in points from the top-left of the page. The PDF coordinate system has its
// origin at bottom-left, but gopdf's SetXY/Text use a top-left origin, so all
// coordinates in this file are top-left-based.
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
	st, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("stat template: %w", err)
	}
	// Key the cache — both the in-process map and the temp file's own name —
	// on the template's size and mtime, not just its path. The finance office
	// re-issues this form under the same filename, and a temp file left over
	// from the previous edition would otherwise keep being served: os.Stat
	// finds it, and every render silently uses the old layout until someone
	// clears /tmp.
	key := fmt.Sprintf("%s|%d|%d", src, st.Size(), st.ModTime().UnixNano())

	normalizedMu.Lock()
	defer normalizedMu.Unlock()
	if cached, ok := normalized[key]; ok {
		if _, err := os.Stat(cached); err == nil {
			return cached, nil
		}
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("ta-creditor-tpl-%d-%d.pdf", st.Size(), st.ModTime().Unix()))
	// gofpdi only reads plain PDF 1.4-style structure: no cross-reference
	// streams and no object streams. Force pdfcpu to emit that shape.
	conf := pdfcpuModel.NewDefaultConfiguration()
	conf.WriteObjectStream = false
	conf.WriteXRefStream = false
	if err := pdfcpu.OptimizeFile(src, tmp, conf); err != nil {
		return "", fmt.Errorf("normalize template: %w", err)
	}
	normalized[key] = tmp
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

// point is a single target position on the form.
type point struct{ x, y float64 }

// circle is one of the three prefixes inside "(นาย/นาง/นางสาว)": the centre of
// the word and the horizontal radius of the ellipse drawn around it. They
// differ because "นางสาว" is nearly twice as wide as "นาย".
type circle struct{ cx, rx float64 }

// digitGrid is a row of printed boxes that take exactly one digit each.
// firstCX is the centre of box 1's white interior; baseline is where a digit's
// baseline has to sit for the glyph to be centred in the box.
type digitGrid struct {
	firstCX  float64
	pitch    float64
	count    int
	baseline float64
}

// field is one value written along a printed run of dots: where the run
// starts, its baseline, and how long it is. The width matters because some
// runs are short — the "(………)" under the signature is ~142 pt — and a long
// Thai name written at full size would run straight into whatever sits next
// to it. See setTextInField.
type field struct{ x, base, w float64 }

// coords holds every overlay position, in points from the top-left of the
// page. Text positions are baselines (see setText) and box positions are the
// centre of the box's white interior, so each value can be read straight off a
// measurement of the blank form — see the recipe on `c` below.
type coords struct {
	// ชื่อ-สกุล row — a ring around one of the three prefixes, then the name
	// on the dotted line.
	prefixes           map[string]circle
	prefixCY, prefixRY float64
	name               field

	// เลขบัตรประจำตัวประชาชน row.
	nid digitGrid

	// โทรศัพท์ติดต่อ / E-Mail row.
	phone, email field

	// ข้อมูลธนาคาร block. The เลขที่บัญชี boxes share the ID row's X grid but
	// there are only ten of them, so they get their own digitGrid rather than
	// borrowing the ID's count.
	accountName, bankName, branchCode, branch field
	accountNo                                 digitGrid

	// Check marks: the interior centre of the box each tick goes in.
	kkuStudent point // ประเภท: "นักศึกษา มข."
	newData    point // ลักษณะการแจ้ง: "เพิ่มข้อมูลใหม่"

	// Signature image, sitting on the ลงชื่อ dotted line, and the printed name
	// centred in the "(………)" below it.
	sigX, sigBottom, sigW, sigH float64
	printedName                 field

	// วันที่ … เดือน … พ.ศ. … — each centred on its own run of dots.
	day, month, year field
}

// c is the calibrated set of positions for the edition of the form currently
// in assets/creditor_form_template.pdf (the "ERP" revision — ส่วนที่ 2 has three
// columns and there is no separate ส่วนที่ 3 กองคลัง).
//
// How these were measured, and how to redo it when the form is re-issued:
//
//  1. Text baselines come from the template's own text runs. pdfplumber
//     reports each run's `bottom`, and THSarabunPSK's descender is 0.25 em,
//     so baseline = bottom − 0.25 × fontsize. Values on a dotted field are
//     the baseline of that field's row of dots, so our text sits on the line
//     exactly the way handwriting would.
//  2. Box positions (the ID / account digit grids and the checkboxes) come
//     from a 4×-scale render of the blank page: walk the mid scanline of the
//     row, and each white run bounded by the printed rule is one box's
//     writable interior. That gives the interior centre directly, which is
//     what a centred digit or tick needs — reading the glyph's own advance
//     width instead would put everything a point or two off, because the box
//     glyph is not centred in its em.
//  3. Re-render with Debug=true (?grid=1 on the preview endpoint) to
//     eyeball the result; the grid is 10 pt minor / 50 pt major with labels.
//
// The template page is 595 × 842 pt and gets stretched onto the A4 output
// page (595.28 × 841.89), i.e. by +0.05% across and −0.01% down. That is at
// most a quarter of a point at the far edge — an order of magnitude below the
// tolerance of these fields — so measured template points are used as-is.
var c = coords{
	// ชื่อ-สกุล row: dots baseline 98.4, "(นาย/นาง/นางสาว)" spanning
	// 181.8–263.7 with a glyph centre line at 94.4. Each ring is centred on
	// its own word and sized to it.
	prefixes: map[string]circle{
		"นาย":    {cx: 193.7, rx: 13.0}, // word spans 184.8–202.6
		"นาง":    {cx: 215.2, rx: 12.5}, // 206.9–223.4
		"นางสาว": {cx: 244.2, rx: 20.5}, // 227.7–260.7
	},
	prefixCY: 94.4,
	prefixRY: 9,
	name:     field{x: 273, base: 98.4, w: 270}, // dots run 270.7–544.1

	// 13 boxes, interiors 12.6 × 16.1 pt centred on y=174.2, first at x=188.8
	// with a 20.99 pt pitch. A digit's cap height at 11 pt is ~7.7, so its
	// baseline sits half of that below the interior centre.
	nid: digitGrid{firstCX: 188.8, pitch: 20.99, count: 13, baseline: 178.1},

	// โทรศัพท์ติดต่อ / E-Mail row, dots baseline 220.4.
	phone: field{x: 181, base: 220.4, w: 150}, // dots 179.6–332.5
	email: field{x: 384, base: 220.4, w: 158}, // dots 382.7–543.4

	// Bank block — every value sits on its own row of dots, all starting at
	// x=178.3 except ชื่อสาขา which starts at 327.8.
	accountName: field{x: 180, base: 268.8, w: 290}, // dots 178.3–470.8
	bankName:    field{x: 180, base: 314.0, w: 292}, // dots 178.3–473.0
	branchCode:  field{x: 180, base: 338.0, w: 80},  // dots 178.3–261.3
	branch:      field{x: 329, base: 338.0, w: 156}, // dots 327.8–486.5
	// Same X grid as the ID row, ten boxes instead of thirteen, interiors
	// centred on y=359.2.
	accountNo: digitGrid{firstCX: 188.8, pitch: 20.99, count: 10, baseline: 363.1},

	// Checkbox interiors are 6.9 × 8.9 pt. ประเภท row centres: บุคลากร 184.2,
	// กรรมการ 264.0, นักศึกษา 484.6 — all at y=114.4. ลักษณะการแจ้ง row:
	// เพิ่มใหม่ 184.2, แก้ไข 268.7, เปลี่ยนบัญชี 398.2 — all at y=138.9.
	kkuStudent: point{x: 484.6, y: 114.4},
	newData:    point{x: 184.2, y: 138.9},

	// The ลงชื่อ dots run 331.5–472.5 on baseline 448.1; the signature image
	// stands on that line, centred on the dots.
	sigX: 347, sigBottom: 448.1, sigW: 110, sigH: 28,
	// "(………)" below it, on baseline 469.2. The width here is the room between
	// the brackets — "(" ends at 335.4 and ")" starts at 472.7 — not the whole
	// printed run, so a long name shrinks to fit inside them instead of
	// printing over them. This is the tightest run on the form, and the one
	// that actually exercises the shrink-to-fit in setTextInField.
	printedName: field{x: 336, base: 469.2, w: 136},

	// วันที่ … เดือน … พ.ศ. … on baseline 514.1, each value centred on its own
	// run of dots.
	day:   field{x: 313.9, base: 514.1, w: 28.6},
	month: field{x: 371.8, base: 514.1, w: 64.8},
	year:  field{x: 462.1, base: 514.1, w: 28.5},
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

	if err := pdf.SetFont("sarabun", "", bodyFont); err != nil {
		return nil, err
	}
	pdf.SetLineWidth(0.8)
	pdf.SetStrokeColor(0, 0, 0)
	pdf.SetFillColor(0, 0, 0)
	pdf.SetTextColor(0, 0, 0)

	// Ring the prefix. An unrecognised value simply leaves all three unringed
	// rather than guessing — the profile form only offers these three.
	if ring, ok := c.prefixes[d.Prefix]; ok {
		drawOval(&pdf, ring.cx, c.prefixCY, ring.rx, c.prefixRY)
	}

	setTextInField(&pdf, c.name, d.FullName, alignLeft)

	fillGrid(&pdf, c.nid, onlyDigits(d.NationalID))

	setTextInField(&pdf, c.phone, d.Phone, alignLeft)
	setTextInField(&pdf, c.email, d.Email, alignLeft)
	setTextInField(&pdf, c.accountName, d.AccountName, alignLeft)
	// The label on the form already reads "ชื่อธนาคาร", so strip a leading
	// "ธนาคาร" from the stored name ("ธนาคารไทยพาณิชย์" → "ไทยพาณิชย์").
	setTextInField(&pdf, c.bankName,
		strings.TrimSpace(strings.TrimPrefix(d.BankName, "ธนาคาร")), alignLeft)
	setTextInField(&pdf, c.branchCode, d.BranchCode, alignLeft)
	setTextInField(&pdf, c.branch, d.Branch, alignLeft)

	fillGrid(&pdf, c.accountNo, onlyDigits(d.AccountNo))

	// Bump the stroke width just for the checkmarks so the tick reads clearly
	// on the small checkboxes on the form. Reset immediately after.
	pdf.SetLineWidth(1.4)
	drawCheck(&pdf, c.kkuStudent)
	drawCheck(&pdf, c.newData)
	pdf.SetLineWidth(0.8)

	if len(d.SignaturePNG) > 0 {
		if err := placePNG(&pdf, d.SignaturePNG, c.sigX, c.sigBottom-c.sigH, c.sigW, c.sigH); err != nil {
			return nil, fmt.Errorf("place signature: %w", err)
		}
	}

	// Printed full name (with prefix, Thai style — no space after prefix)
	// inside the "(………)" parens under the signature line. Guarded on the name
	// rather than on the joined string, so a profile with only a prefix filled
	// in doesn't print a bare "นาย" on the signature line.
	if d.FullName != "" {
		setTextInField(&pdf, c.printedName, d.Prefix+d.FullName, alignCenter)
	}

	setTextInField(&pdf, c.day, d.Day, alignCenter)
	setTextInField(&pdf, c.month, d.Month, alignCenter)
	setTextInField(&pdf, c.year, d.Year, alignCenter)

	if in.Debug {
		drawGrid(&pdf)
	}

	var buf bytes.Buffer
	if _, err := pdf.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// setText draws s with its baseline at y. gopdf.Text (unlike Cell, which takes
// the top-left of a line box and so needs a font-size-dependent fudge factor)
// treats the current Y as the baseline — matching how every Y in `c` was
// measured off the blank form.
func setText(pdf *gopdf.GoPdf, x, y float64, s string) {
	if s == "" {
		return
	}
	pdf.SetXY(x, y)
	_ = pdf.Text(s)
}

// setTextCentered draws s centred horizontally on cx.
func setTextCentered(pdf *gopdf.GoPdf, cx, y float64, s string) {
	if s == "" {
		return
	}
	w := measure(pdf, s)
	setText(pdf, cx-w/2, y, s)
}

// measure is MeasureTextWidth with the error folded away: it only fails on
// glyphs the font subset can't take, in which case Text() would drop them too,
// and treating the run as zero-width still puts the rest of the value on the
// page rather than losing the field entirely.
func measure(pdf *gopdf.GoPdf, s string) float64 {
	w, err := pdf.MeasureTextWidth(s)
	if err != nil {
		return 0
	}
	return w
}

const (
	// dottedLift is how far above a printed dotted line a value's baseline
	// goes. Thai below-vowels (ุ ู) and the ์ mark descend ~2 pt at bodyFont,
	// so writing exactly on the dots' own baseline draws them straight through
	// those glyphs; a small lift reads as handwriting resting on the line.
	dottedLift = 2.5
	// bodyFont is the size every value is written at, and minFont the floor
	// setTextInField will shrink to before giving up and letting a value
	// overrun. Below ~8 pt Sarabun's Thai marks stop being legible in print,
	// and an overrun is easier for staff to spot and fix than a line they
	// can't read at all.
	bodyFont = 11.0
	minFont  = 8.0
)

type align int

const (
	alignLeft align = iota
	alignCenter
)

// setTextInField writes s along f's run of dots, shrinking the font a step at
// a time if the value is too wide for the run. Names on this form regularly
// outgrow the short runs — "(………)" under the signature is only ~142 pt — and
// overrunning into the neighbouring cell reads as a broken document, whereas a
// slightly smaller line just reads as a long name.
func setTextInField(pdf *gopdf.GoPdf, f field, s string, a align) {
	if s == "" {
		return
	}
	size := bodyFont
	for measure(pdf, s) > f.w && size > minFont {
		size -= 0.5
		if err := pdf.SetFont("sarabun", "", size); err != nil {
			break
		}
	}
	y := f.base - dottedLift
	if a == alignCenter {
		setTextCentered(pdf, f.x+f.w/2, y, s)
	} else {
		setText(pdf, f.x, y, s)
	}
	if size != bodyFont {
		_ = pdf.SetFont("sarabun", "", bodyFont)
	}
}

// fillGrid writes one digit per printed box, each centred in its box. Digits
// past the last box are dropped: the grid is as long as the form allows, and
// spilling past the printed row would look like a rendering fault rather than
// a data problem.
func fillGrid(pdf *gopdf.GoPdf, g digitGrid, digits string) {
	for i, r := range digits {
		if i >= g.count {
			break
		}
		setTextCentered(pdf, g.firstCX+float64(i)*g.pitch, g.baseline, string(r))
	}
}

func drawOval(pdf *gopdf.GoPdf, cx, cy, rx, ry float64) {
	pdf.Oval(cx-rx, cy-ry, cx+rx, cy+ry)
}

// drawCheck draws a "✓" centred on p using two line strokes. The form's
// checkboxes have a ~7 × 9 pt interior, so the tick is sized to overhang it
// slightly the way a pen would rather than to fit primly inside the frame.
func drawCheck(pdf *gopdf.GoPdf, p point) {
	const w, h = 10.0, 9.0
	l, t := p.x-w/2, p.y-h/2
	pdf.Line(l, t+h*0.55, l+w*0.35, t+h)
	pdf.Line(l+w*0.35, t+h, l+w, t)
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
