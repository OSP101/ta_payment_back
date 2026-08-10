package service

import (
	"math"
	"strings"
)

// bahttext.go converts a baht amount into the Thai text a signed document
// prints ("...บาทถ้วน"), the same convention as a Thai bank cheque. Needed by
// the ปะหน้าจ่ายตรง transfer-cover sheet, which prints the total both as a
// number and spelled out.

var thaiDigitWords = [...]string{
	"ศูนย์", "หนึ่ง", "สอง", "สาม", "สี่", "ห้า", "หก", "เจ็ด", "แปด", "เก้า",
}

// thaiPlaceWords are the place-value suffixes for a 6-digit group, indexed by
// position (0 = units, 5 = hundred-thousands). Position 0 never gets a suffix.
var thaiPlaceWords = [...]string{"", "สิบ", "ร้อย", "พัน", "หมื่น", "แสน"}

// readGroup spells out n (0..999999) — a six-digit chunk, or the whole amount
// when it fits in one — with the standard place-value exceptions:
//   - a tens digit of 1 drops the digit word ("สิบ", never "หนึ่งสิบ")
//   - a tens digit of 2 becomes "ยี่สิบ", never "สองสิบ"
//   - the UNITS digit becomes "เอ็ด" instead of "หนึ่ง", but only when n
//     itself is 10 or more — "1" alone still reads "หนึ่ง".
//
// That last rule is LOCAL to whichever group is currently being read, which
// is what makes chunked million-groups work without any extra bookkeeping:
// each chunk is pronounced exactly as if it were read on its own before the
// "ล้าน" that follows it. 21 million is "ยี่สิบเอ็ดล้าน" because 21, read alone,
// is "ยี่สิบเอ็ด" — but 1 million stays "หนึ่งล้าน", because 1, read alone, is
// "หนึ่ง", not "เอ็ด". The same rule then separately decides the units chunk
// below it (1,000,011 → "หนึ่งล้านสิบเอ็ด": the trailing chunk, 11, is read on
// its own merits, unrelated to the chunk above it).
//
// A zero digit at any position is simply skipped; there is no spoken "ศูนย์"
// in the middle of a number, only for the number zero itself.
func readGroup(n int) string {
	if n == 0 {
		return ""
	}
	allowEd := n >= 10
	digits := [6]int{}
	for i := 0; i < 6; i++ {
		digits[i] = n % 10
		n /= 10
	}
	var b strings.Builder
	for pos := 5; pos >= 0; pos-- {
		d := digits[pos]
		if d == 0 {
			continue
		}
		switch {
		case pos == 1 && d == 1:
			b.WriteString("สิบ")
		case pos == 1 && d == 2:
			b.WriteString("ยี่สิบ")
		case pos == 0 && d == 1 && allowEd:
			b.WriteString("เอ็ด")
		default:
			b.WriteString(thaiDigitWords[d])
			b.WriteString(thaiPlaceWords[pos])
		}
	}
	return b.String()
}

// readInteger spells out an arbitrary non-negative integer, chunked into
// groups of 6 digits. Thai has no word beyond ล้าน — a further group of 6
// just repeats it, so a chunk k positions up gets "ล้าน" written k times
// (the millions chunk once, the "million millions" chunk twice, and so on).
func readInteger(n int64) string {
	if n == 0 {
		return thaiDigitWords[0]
	}
	var chunks []string
	k := 0
	for n > 0 {
		chunk := int(n % 1_000_000)
		n /= 1_000_000
		if chunk != 0 {
			chunks = append([]string{readGroup(chunk) + strings.Repeat("ล้าน", k)}, chunks...)
		}
		k++
	}
	return strings.Join(chunks, "")
}

// BahtText spells out a baht amount the way it prints on a signed transfer
// document: "...บาทถ้วน" when the amount is a whole number of baht, or
// "...บาท...สตางค์" when it carries satang. Negative amounts are prefixed
// "ลบ" — a payout total should never be negative, but a document that silently
// dropped the sign on a bad input would be worse than one that says so.
func BahtText(baht float64) string {
	neg := baht < 0
	if neg {
		baht = -baht
	}
	// Round to the nearest satang first: baht arrives as a float, and reading
	// digits off an unrounded float64 risks "0.1"-style representation error
	// turning ".10" into ".09999...".
	totalSatang := int64(math.Round(baht * 100))
	bahtPart := totalSatang / 100
	satangPart := totalSatang % 100

	var b strings.Builder
	if neg {
		b.WriteString("ลบ")
	}
	b.WriteString(readInteger(bahtPart))
	b.WriteString("บาท")
	if satangPart == 0 {
		b.WriteString("ถ้วน")
	} else {
		b.WriteString(readGroup(int(satangPart)))
		b.WriteString("สตางค์")
	}
	return b.String()
}
