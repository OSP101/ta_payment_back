package service

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	excel "github.com/jloor/go-excel-reader"
)

var oleWorkbookMagic = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}

// IsLegacyREGWorkbook checks the container, not the supplied file name.
func IsLegacyREGWorkbook(body []byte) bool { return bytes.HasPrefix(body, oleWorkbookMagic) }

// The BIFF reader can panic for a missing row or malformed legacy record. Keep
// those failures inside this adapter and bound the materialized cell grid.
func readLegacyREGRows(body []byte) (rows [][]string, err error) {
	defer func() {
		if recover() != nil {
			rows, err = nil, Invalid(errImportUnreadableMsg)
		}
	}()
	if len(body) > 20<<20 {
		return nil, Invalid("ไฟล์ใหญ่เกิน 20 MB")
	}
	r, e := excel.OpenXLS(bytes.NewReader(body), excel.WithErrorValues(true))
	if e != nil {
		return nil, Invalid(errImportUnreadableMsg)
	}
	defer r.Close()
	found := false
	for r.NextSheet() {
		if !strings.EqualFold(r.SheetName(), "sysTitle") {
			continue
		}
		found = true
		for r.Read() {
			index := r.RowIndex()
			if index >= 20000 {
				return nil, Invalid("ไฟล์มีมากกว่า 20,000 แถว กรุณาแยกไฟล์")
			}
			if r.FieldCount() > 100 {
				return nil, Invalid(fmt.Sprintf("แถว %d มีคอลัมน์มากเกินรูปแบบ REG", index+1))
			}
			for len(rows) <= index {
				rows = append(rows, nil)
			}
			cells := make([]string, r.FieldCount())
			for c := range cells {
				switch v := r.GetValue(c).(type) {
				case nil:
				case string:
					cells[c] = v
				case int64:
					cells[c] = strconv.FormatInt(v, 10)
				case float64:
					cells[c] = strconv.FormatFloat(v, 'f', -1, 64)
				default:
					cells[c] = fmt.Sprint(v)
				}
			}
			rows[index] = cells
		}
		break
	}
	if r.Err() != nil {
		return nil, Invalid(errImportUnreadableMsg)
	}
	if !found {
		return nil, Invalid("ไม่พบชีต sysTitle ในไฟล์ REG")
	}
	return rows, nil
}

func sectionImportKey(no, track string) string {
	if track == "special" {
		return no + "|special"
	}
	return no
}

func regTrack(reserved string) string {
	label := strings.SplitN(reserved, ":", 2)[0]
	if strings.Contains(label, "พิเศษ") {
		return "special"
	}
	return "regular"
}
