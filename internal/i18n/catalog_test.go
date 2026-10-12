package i18n

import (
	"encoding/json"
	"regexp"
	"sort"
	"testing"
)

// Every catalog entry must keep its format verbs: an English value that drops
// or adds one would print %!s(MISSING) or swallow a value.
func TestCatalogVerbsMatch(t *testing.T) {
	var raw map[string]string
	if err := json.Unmarshal(catalogJSON, &raw); err != nil {
		t.Fatalf("catalog_en.json: %v", err)
	}
	strip := regexp.MustCompile(`\[\d+\]`)
	verbs := func(s string) []string {
		var out []string
		for _, v := range verb.FindAllString(s, -1) {
			out = append(out, strip.ReplaceAllString(v, ""))
		}
		sort.Strings(out)
		return out
	}
	for th, en := range raw {
		a, b := verbs(th), verbs(en)
		if len(a) != len(b) {
			t.Errorf("%q → %q: verbs %v vs %v", th, en, a, b)
			continue
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%q → %q: verbs %v vs %v", th, en, a, b)
				break
			}
		}
	}
}

// Real messages from the code, end to end through the embedded catalog.
func TestCatalogRealMessages(t *testing.T) {
	for _, th := range []string{"ข้อมูลไม่ถูกต้อง", "ไม่พบข้อมูลที่ต้องการ", "คุณไม่มีสิทธิ์ดำเนินการนี้"} {
		if got := T("en", th); got == th {
			t.Errorf("%q not translated", th)
		}
	}
}
