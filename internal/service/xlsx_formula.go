package service

import "fmt"

// xlFormula is a formula the export code wrote itself, e.g. "=SUM(H10:H20)".
//
// The workbook writers used to treat ANY string starting with "=" as a formula,
// so a name typed into the system as `=HYPERLINK("http://…","x")` — a lecturer
// can create a TA account with any name — became a live formula in a claim
// document opened by finance. Only a value of this type is ever written with
// SetCellFormula; every plain string, whatever it starts with, is written as
// text and shows exactly as typed.
type xlFormula string

// xlf builds an xlFormula the way the call sites used fmt.Sprintf.
func xlf(format string, a ...any) xlFormula { return xlFormula(fmt.Sprintf(format, a...)) }
