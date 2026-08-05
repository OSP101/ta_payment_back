// draw.go holds the tiny drawing primitives every generated-from-scratch PDF
// in this package shares — the appointment order and the timetable form. They
// assume the caller already registered the Sarabun faces under the "sarabun"
// and "sarabunb" names.
package pdfgen

import "github.com/signintech/gopdf"

func setBold(pdf *gopdf.GoPdf, size float64) {
	_ = pdf.SetFont("sarabunb", "", size)
}
func setReg(pdf *gopdf.GoPdf, size float64) {
	_ = pdf.SetFont("sarabun", "", size)
}
func textAt(pdf *gopdf.GoPdf, x, y float64, s string) {
	if s == "" {
		return
	}
	pdf.SetXY(x, y)
	_ = pdf.Cell(nil, s)
}
