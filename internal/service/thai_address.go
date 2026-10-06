package service

// thai_address.go — the province → district → sub-district list behind the
// address pickers on the TA profile form (06/10/2026). Embedded rather than
// fetched from a government API at form time: the list barely changes, and a
// form that depends on someone else's server being up is a form TAs cannot
// send when it is down.
//
// data/thai_address.json is derived from github.com/kongvut/thai-province-data
// (MIT, see data/thai_address.LICENSE), compacted to
//
//	{"p": [[provinceID, "ชื่อ", [[districtID, "ชื่อ", [[subDistrictID, "ชื่อ", zip], ...]], ...]], ...]}
//
// The same bytes are served to the browser (ThaiAddressJSON), so the picker
// and this validator can never disagree about what exists.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

//go:embed data/thai_address.json
var thaiAddressJSON []byte

// bangkokProvinceID uses เขต/แขวง instead of อำเภอ/ตำบล.
const bangkokProvinceID = 1

type ThaiSubDistrict struct {
	ID         int
	Name       string
	Zip        string
	District   string
	Province   string
	ProvinceID int
}

var (
	thaiAddressOnce sync.Once
	thaiAddressByID map[int]ThaiSubDistrict
)

func loadThaiAddress() {
	var raw struct {
		P [][]json.RawMessage `json:"p"`
	}
	if err := json.Unmarshal(thaiAddressJSON, &raw); err != nil {
		panic("thai_address.json: " + err.Error())
	}
	m := make(map[int]ThaiSubDistrict, 7500)
	for _, p := range raw.P {
		var pid int
		var pname string
		var districts [][]json.RawMessage
		_ = json.Unmarshal(p[0], &pid)
		_ = json.Unmarshal(p[1], &pname)
		_ = json.Unmarshal(p[2], &districts)
		for _, d := range districts {
			var dname string
			var subs [][3]any
			_ = json.Unmarshal(d[1], &dname)
			_ = json.Unmarshal(d[2], &subs)
			for _, s := range subs {
				id := int(s[0].(float64))
				m[id] = ThaiSubDistrict{
					ID: id, Name: s[1].(string), Zip: fmt.Sprintf("%05d", int(s[2].(float64))),
					District: dname, Province: pname, ProvinceID: pid,
				}
			}
		}
	}
	thaiAddressByID = m
}

// ThaiAddressJSON is the list as served to the browser.
func ThaiAddressJSON() []byte { return thaiAddressJSON }

// LookupSubDistrict finds a sub-district by its 6-digit code.
func LookupSubDistrict(id int) (ThaiSubDistrict, bool) {
	thaiAddressOnce.Do(loadThaiAddress)
	sd, ok := thaiAddressByID[id]
	return sd, ok
}

// FormatThaiAddress joins the typed part (บ้านเลขที่ หมู่ ถนน) with the picked
// names the way the finance sample writes it:
// "123 ม.4 ต.ท่าพระ อ.เมืองขอนแก่น จ.ขอนแก่น", and in Bangkok
// "99 ถ.พญาไท แขวงทุ่งพญาไท เขตราชเทวี กรุงเทพมหานคร".
func FormatThaiAddress(line string, sd ThaiSubDistrict) string {
	line = strings.Join(strings.Fields(line), " ")
	var tail string
	if sd.ProvinceID == bangkokProvinceID {
		tail = "แขวง" + sd.Name + " เขต" + sd.District + " " + sd.Province
	} else {
		tail = "ต." + sd.Name + " อ." + sd.District + " จ." + sd.Province
	}
	if line == "" {
		return tail
	}
	return line + " " + tail
}
