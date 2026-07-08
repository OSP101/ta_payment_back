// Package docxgen fills the "แบบแจ้งข้อมูลเจ้าหนี้บุคลากร" DOCX template with
// TA profile data and optionally embeds a signature PNG.
//
// Approach: DOCX is a zip. We copy every entry, and for `word/document.xml`
// we perform surgical text substitution matching the exact placeholder runs
// present in the source template (which was rendered by KKU's finance office).
// This avoids a heavy DOCX library while preserving every style, font (Sarabun),
// table layout, and image already in the template.
//
// If SignaturePNG is provided, we:
//   1. Write it as `word/media/signature.png` in the output zip.
//   2. Add a Relationship pointing to it in `word/_rels/document.xml.rels`.
//   3. Replace the "ลงชื่อ ………" run with a drawing (inline image) fragment
//      that references the signature via the new Relationship.
package docxgen

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Data is the payload used to fill the creditor form.
type Data struct {
	FullName    string
	NationalID  string // 13 digits, dashes stripped
	Phone       string
	Email       string
	AccountName string
	BankName    string
	BranchCode  string
	Branch      string
	AccountNo   string
	Day         string // Thai calendar day (e.g. "7")
	Month       string // Thai month name (e.g. "กรกฎาคม")
	Year        string // Buddhist year (e.g. "2569")

	// Raw PNG bytes for the signature. Optional. If empty a line is drawn.
	SignaturePNG []byte
}

// Fill returns the filled DOCX bytes.
func Fill(templatePath string, d Data) ([]byte, error) {
	src, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}
	return FillBytes(src, d)
}

// FillBytes is like Fill but takes template bytes directly.
func FillBytes(src []byte, d Data) ([]byte, error) {
	if d.Day == "" || d.Month == "" || d.Year == "" {
		now := time.Now()
		d.Day = fmt.Sprintf("%d", now.Day())
		d.Month = thaiMonth(int(now.Month()))
		d.Year = fmt.Sprintf("%d", now.Year()+543)
	}

	zr, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}

	var out bytes.Buffer
	zw := zip.NewWriter(&out)

	haveSig := len(d.SignaturePNG) > 0
	const sigRelID = "rIdSignatureTA"

	for _, f := range zr.File {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     f.Name,
			Method:   f.Method,
			Modified: f.Modified,
		})
		if err != nil {
			return nil, err
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}

		switch f.Name {
		case "word/document.xml":
			body = substituteDocument(body, d, haveSig, sigRelID)
		case "word/_rels/document.xml.rels":
			if haveSig {
				body = addSignatureRel(body, sigRelID, "media/signature.png")
			}
		case "[Content_Types].xml":
			if haveSig {
				body = ensurePNGContentType(body)
			}
		}

		if _, err := w.Write(body); err != nil {
			return nil, err
		}
	}
	if haveSig {
		w, err := zw.Create("word/media/signature.png")
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(d.SignaturePNG); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// substituteDocument performs the text replacements in word/document.xml.
// These target strings match exactly what is in the source template.
func substituteDocument(body []byte, d Data, haveSig bool, sigRelID string) []byte {
	s := string(body)

	// Row 2: full name  — "(นาย/นาง/นางสาว) …………"
	s = replaceOnce(s,
		"(นาย/นาง/นางสาว) …………………………………………………………………………..…………",
		"(นาย/นาง/นางสาว) "+xmlEscape(d.FullName))

	// Row 5: national ID — the cell is empty; we insert a spaced 13-digit string
	// after the label. Match the label and append 13-digit boxes.
	nid := formatNationalID(d.NationalID)
	s = replaceOnce(s,
		"เลขบัตรประจ าตัวประชาชน/เลข ประจ าตัวผู้เสียภาษี:",
		"เลขบัตรประจ าตัวประชาชน/เลข ประจ าตัวผู้เสียภาษี:  "+xmlEscape(nid))

	// Row 6: phone + email
	// Two separate strings in different cells — they use ASCII dots not …
	s = replaceOnce(s,
		".............................................................",
		xmlEscape(d.Phone))
	s = replaceOnce(s,
		"E-Mail: ..............................................................",
		"E-Mail: "+xmlEscape(d.Email))

	// Row 7: bank info — each label ("ชื่อบัญชี", "ชื่อธนาคาร") is in its own
	// text run; the dots are in the following run. Replace the dot-runs:
	s = replaceOnce(s,
		"……………………..................................………………………………………………..  ",
		xmlEscape(d.AccountName)+" ")
	s = replaceOnce(s,
		"…………………..........................………………………………………………………….. ",
		xmlEscape(d.BankName)+" ")
	// "รหัสสาขา " is in one run; the dots + branch name in the next run.
	s = replaceOnce(s,
		"................................ ชื่อสาขา ……......………………………………………….. ",
		xmlEscape(d.BranchCode)+" ชื่อสาขา "+xmlEscape(d.Branch)+" ")

	// Account number — appears as "เลขที่บัญชี" possibly followed by empty digit boxes.
	s = replaceOnce(s, "เลขที่บัญชี", "เลขที่บัญชี "+xmlEscape(d.AccountNo))

	// Row 8: signature line, printed name in parens, and date.
	if haveSig {
		// Replace "ลงชื่อ ………" with a drawing referring to signature.png
		signLine := "ลงชื่อ ………………...……………………..…"
		signDraw := "ลงชื่อ " + inlineImageXML(sigRelID, 3000000, 900000) // ~3cm x 1cm
		s = replaceOnce(s, signLine, signDraw)
	} else {
		s = replaceOnce(s,
			"ลงชื่อ ………………...……………………..…",
			"ลงชื่อ ______________________________")
	}
	// Printed name under signature
	s = replaceOnce(s,
		"(……….………………………………………..)",
		"("+xmlEscape(d.FullName)+")")

	// Date row
	s = replaceOnce(s,
		"วันที่ ........... เดือน ......................... พ.ศ. ...........",
		"วันที่ "+xmlEscape(d.Day)+" เดือน "+xmlEscape(d.Month)+" พ.ศ. "+xmlEscape(d.Year))

	return []byte(s)
}

// addSignatureRel appends a Relationship for the signature image into the
// document.xml.rels file, and returns the updated body.
func addSignatureRel(body []byte, relID, target string) []byte {
	s := string(body)
	insertion := fmt.Sprintf(
		`<Relationship Id="%s" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="%s"/>`,
		relID, target)
	// Insert just before </Relationships>
	if i := strings.LastIndex(s, "</Relationships>"); i > 0 {
		s = s[:i] + insertion + s[i:]
	}
	return []byte(s)
}

// ensurePNGContentType makes sure [Content_Types].xml declares png so Word
// renders the embedded image. The stock template usually already has this
// because it contains other PNG assets, but we add it defensively.
func ensurePNGContentType(body []byte) []byte {
	s := string(body)
	if strings.Contains(s, `Extension="png"`) {
		return body
	}
	insertion := `<Default Extension="png" ContentType="image/png"/>`
	if i := strings.Index(s, "<Types "); i > 0 {
		end := strings.Index(s[i:], ">")
		if end > 0 {
			at := i + end + 1
			s = s[:at] + insertion + s[at:]
		}
	}
	return []byte(s)
}

// inlineImageXML returns the OOXML fragment for an inline drawing.
// cx/cy are EMU (English Metric Units); 914400 EMU = 1 inch.
func inlineImageXML(relID string, cxEMU, cyEMU int) string {
	return fmt.Sprintf(`</w:t></w:r><w:r><w:drawing><wp:inline distT="0" distB="0" distL="0" distR="0" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"><wp:extent cx="%d" cy="%d"/><wp:docPr id="1" name="Signature"/><wp:cNvGraphicFramePr/><a:graphic xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:nvPicPr><pic:cNvPr id="1" name="Signature"/><pic:cNvPicPr/></pic:nvPicPr><pic:blipFill><a:blip xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" r:embed="%s"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill><pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"/></pic:spPr></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r><w:r><w:t xml:space="preserve">`,
		cxEMU, cyEMU, relID, cxEMU, cyEMU)
}

// formatNationalID adds spaces between digits so each digit reads clearly
// (e.g. "1 2 3 4 5 6 7 8 9 0 1 2 3").
func formatNationalID(id string) string {
	// Strip non-digits.
	digits := make([]rune, 0, 13)
	for _, c := range id {
		if c >= '0' && c <= '9' {
			digits = append(digits, c)
		}
	}
	if len(digits) == 0 {
		return "-"
	}
	return strings.Join(splitDigits(digits), " ")
}

func splitDigits(rs []rune) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}

func replaceOnce(s, old, new string) string {
	i := strings.Index(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	)
	return r.Replace(s)
}

func thaiMonth(m int) string {
	names := []string{"", "มกราคม", "กุมภาพันธ์", "มีนาคม", "เมษายน", "พฤษภาคม", "มิถุนายน",
		"กรกฎาคม", "สิงหาคม", "กันยายน", "ตุลาคม", "พฤศจิกายน", "ธันวาคม"}
	if m < 1 || m > 12 {
		return ""
	}
	return names[m]
}
