// Package i18n puts the API's Thai messages into English for readers who
// chose English (users.ui_locale, mirrored in the ui_locale cookie).
//
// Thai stays the source language: the code keeps writing Thai, and this
// package translates on the way out — error responses and in-app
// notifications. Anything not in the catalog goes out in Thai, so a missing
// entry is never worse than before. Official documents and e-mail are not
// translated (the faculty's forms are Thai).
//
// catalog_en.json maps a Thai string to English. A key may be a fmt format
// ("ขาด %d ชั่วโมง"): it then matches any message that format can produce,
// and the captured values are put into the English in the same order (or
// by position with %[n]s). `go run ./cmd/i18n-extract -missing` lists what
// still needs an entry.
package i18n

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed catalog_en.json
var catalogJSON []byte

type pattern struct {
	re     *regexp.Regexp
	en     string // English with %s-style verbs, in the order of the Thai verbs
	litLen int    // literal characters in the key: longer = more specific, tried first
	lines  bool   // the key itself spans lines
}

var (
	once     sync.Once
	exact    map[string]string
	patterns []pattern
)

var verb = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(?:\.\d+)?[vsdqfgxXtwT]`)

func load() {
	exact = map[string]string{}
	raw := map[string]string{}
	_ = json.Unmarshal(catalogJSON, &raw)
	for th, en := range raw {
		if en == "" {
			continue
		}
		if !verb.MatchString(th) {
			exact[strings.TrimSpace(th)] = en
			continue
		}
		addPattern(th, en)
	}
	sort.SliceStable(patterns, func(i, j int) bool { return patterns[i].litLen > patterns[j].litLen })
}

func addPattern(th, en string) {
	{
		var b strings.Builder
		b.WriteString("^")
		last, lit := 0, 0
		for _, loc := range verb.FindAllStringIndex(th, -1) {
			chunk := strings.ReplaceAll(th[last:loc[0]], "%%", "%")
			lit += len(chunk)
			b.WriteString(regexp.QuoteMeta(chunk))
			v := th[loc[0]:loc[1]]
			switch v[len(v)-1] {
			case 'd':
				b.WriteString(`(-?\d+)`)
			case 'f', 'g':
				b.WriteString(`(-?[\d.,]+)`)
			default:
				b.WriteString(`(.+?)`)
			}
			last = loc[1]
		}
		tail := strings.ReplaceAll(th[last:], "%%", "%")
		lit += len(tail)
		b.WriteString(regexp.QuoteMeta(tail))
		b.WriteString("$")
		re, err := regexp.Compile("(?s)" + b.String())
		if err != nil || lit == 0 {
			return
		}
		patterns = append(patterns, pattern{re: re, en: en, litLen: lit, lines: strings.Contains(th, "\n")})
	}
}

// T returns msg in the given locale ("en" translates; anything else returns
// msg unchanged).
func T(locale, msg string) string {
	if locale != "en" || msg == "" || !hasThai(msg) {
		return msg
	}
	once.Do(load)
	// Dates first as well: a month and its Buddhist-era year are only
	// converted together, and a format can capture them apart ("… %s %s" over
	// "CP1 กันยายน 2569") or translate a lone month name before the year is
	// seen. The keys hold no dates, so they match the converted text alike.
	return Dates(translate(Dates(msg), 0))
}

func translate(msg string, depth int) string {
	s := strings.TrimSpace(msg)
	if en, ok := exact[s]; ok {
		return en
	}
	// A message of several lines (a notice body: intro, list, closing) is
	// translated line by line first, each line at this same depth so its
	// values still get translated. Only when no line is known is the whole
	// message tried against the keys that themselves span lines. A one-line
	// key never matches it: "…มีผลตั้งแต่วันที่ %s" would take the rest of a
	// notice as its date.
	multiline := strings.Contains(s, "\n")
	if multiline {
		lines := strings.Split(s, "\n")
		changed := false
		for i, line := range lines {
			if t := translate(line, depth); t != strings.TrimSpace(line) {
				lines[i] = t
				changed = true
			}
		}
		if changed {
			return strings.Join(lines, "\n")
		}
	}
	for _, p := range patterns {
		if multiline && !p.lines {
			continue
		}
		m := p.re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		args := m[1:]
		if depth < 2 {
			for i, a := range args {
				args[i] = translate(a, depth+1)
			}
		}
		// A %s that swallowed the rest of a Thai sentence ("รายวิชา %s" over
		// "รายวิชา CP101 ของท่านได้รับ…") would give half-English, half-Thai —
		// worse than leaving the message alone. Names fit well under the cut.
		if swallowed(args) {
			continue
		}
		return fill(p.en, args)
	}
	// A known message with a code or name glued on after a space —
	// "บันทึกเวลาปฏิบัติงานได้รับการอนุมัติ CP321002" — or before it. Not for
	// a message of several lines: cutting at a space there splits a line in
	// two and pushes every other line a level deeper, so the lines are left to
	// the split below.
	if depth < 2 && !multiline {
		words := strings.Split(s, " ")
		for cut := len(words) - 1; cut >= 1 && cut >= len(words)-4; cut-- {
			head := strings.Join(words[:cut], " ")
			if t := translate(head, depth+1); t != head && !hasThai(t) {
				tail := translate(strings.Join(words[cut:], " "), depth+1)
				if swallowed([]string{tail}) {
					break // the tail is a sentence of its own: leave it all Thai
				}
				return t + " " + tail
			}
		}
	}
	// Messages assembled from parts: "เหตุผล: รายละเอียด", lines of a list.
	if depth < 2 {
		for _, sep := range []string{"\n", ": ", " · ", " — "} {
			if strings.Contains(s, sep) {
				parts := strings.Split(s, sep)
				changed := false
				for i, part := range parts {
					t := translate(part, depth+1)
					if t != strings.TrimSpace(part) {
						changed = true
					}
					parts[i] = t
				}
				if changed {
					return strings.Join(parts, sep)
				}
			}
		}
	}
	return msg
}

// fill puts captured values into the English format: %s/%d/%v… in order, or
// %[n]s by position.
func fill(en string, args []string) string {
	next := 0
	return verb.ReplaceAllStringFunc(en, func(v string) string {
		i := next
		if strings.HasPrefix(v, "%[") {
			if n, err := strconv.Atoi(v[2:strings.Index(v, "]")]); err == nil {
				i = n - 1
			}
		} else {
			next++
		}
		if i >= 0 && i < len(args) {
			return args[i]
		}
		return v
	})
}

func hasThai(s string) bool {
	for _, r := range s {
		if r >= 0x0E00 && r <= 0x0E7F {
			return true
		}
	}
	return false
}

// SetForTest replaces the catalog with exact entries, for other packages'
// tests.
func SetForTest(m map[string]string) {
	once.Do(func() {})
	exact = m
	patterns = nil
}

var (
	thMonths     = []string{"มกราคม", "กุมภาพันธ์", "มีนาคม", "เมษายน", "พฤษภาคม", "มิถุนายน", "กรกฎาคม", "สิงหาคม", "กันยายน", "ตุลาคม", "พฤศจิกายน", "ธันวาคม"}
	thMonthsAbbr = []string{"ม.ค.", "ก.พ.", "มี.ค.", "เม.ย.", "พ.ค.", "มิ.ย.", "ก.ค.", "ส.ค.", "ก.ย.", "ต.ค.", "พ.ย.", "ธ.ค."}
	enMonths     = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	enMonthsAbbr = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	monthYear    *regexp.Regexp
	monthOnly    *strings.Replacer
)

func init() {
	var alts []string
	for i := range thMonths {
		alts = append(alts, regexp.QuoteMeta(thMonths[i]), regexp.QuoteMeta(thMonthsAbbr[i]))
	}
	// A month followed by a Buddhist-era year: the year becomes CE.
	monthYear = regexp.MustCompile(`(` + strings.Join(alts, "|") + `)\s*(25\d\d)`)
	var pairs []string
	for i := range thMonths {
		pairs = append(pairs, thMonths[i], enMonths[i], thMonthsAbbr[i], enMonthsAbbr[i])
	}
	monthOnly = strings.NewReplacer(pairs...)
}

// Dates rewrites Thai month names, and a Buddhist-era year written right
// after one, into English: "15 มิถุนายน 2569" → "15 June 2026",
// "มิ.ย.–ก.ย." → "Jun–Sep". Academic-year labels ("2569/1", "ปีการศึกษา
// 2568") are identifiers and are left alone — only a year that follows a
// month is converted.
func Dates(s string) string {
	if !hasThai(s) {
		return s
	}
	s = monthYear.ReplaceAllStringFunc(s, func(m string) string {
		sub := monthYear.FindStringSubmatch(m)
		y, _ := strconv.Atoi(sub[2])
		return sub[1] + " " + strconv.Itoa(y-543)
	})
	return monthOnly.Replace(s)
}

// swallowed reports a captured value that is still a long run of Thai after
// translation: a sentence, not a name or a course title.
func swallowed(args []string) bool {
	for _, a := range args {
		n := 0
		for _, r := range a {
			if r >= 0x0E00 && r <= 0x0E7F && r != 0x0E3F {
				n++
			}
		}
		if n > 30 {
			return true
		}
	}
	return false
}
