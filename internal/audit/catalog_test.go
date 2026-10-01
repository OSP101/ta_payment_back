package audit

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every action the codebase writes must have an entry in the catalog.
//
// Without this, a newly audited action shows up on the audit screen as its raw
// identifier (or, at best, as its family's name — "รายการชั่วโมง" for six
// different things) and cannot be reached by any filter phrased in the
// catalog's terms. That is the state the catalog was written to end: roughly
// 120 action names had no wording of their own.
//
// The check reads the source rather than a hand-kept list, so it cannot be
// satisfied by forgetting to update the list.
func TestCatalogCoversEveryAuditedAction(t *testing.T) {
	literal := regexp.MustCompile(`Action:\s*"([a-z0-9_]+(?:\.[a-z0-9_]+)+)"`)
	// Actions that reach audit.Entry through a variable or a function argument
	// rather than a literal next to `Action:`.
	indirect := regexp.MustCompile(`(?:AuditRead\(aud,|return)\s*"([a-z0-9_]+(?:\.[a-z0-9_]+)+)"`)

	seen := map[string]string{}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "node_modules" || n == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]] = path
		}
		base := filepath.Base(path)
		if base == "router.go" || base == "worklog.go" {
			for _, m := range indirect.FindAllStringSubmatch(string(src), -1) {
				// `return "…"` also matches ordinary strings with a dot in
				// them; only the audit-shaped ones are of interest.
				if base == "worklog.go" && !strings.HasPrefix(m[1], "worklog.") {
					continue
				}
				seen[m[1]] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk source: %v", err)
	}
	if len(seen) < 100 {
		t.Fatalf("found only %d audited actions in the source — the scan is not reading the tree", len(seen))
	}

	var missing []string
	for action, path := range seen {
		if !Describe(action).Known {
			missing = append(missing, action+"  ("+path+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d audited action(s) have no entry in internal/audit/catalog.go:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

func TestCatalogEntriesAreComplete(t *testing.T) {
	cats := map[Category]bool{}
	for _, c := range Categories() {
		cats[c.ID] = true
	}
	for name, i := range catalog {
		if strings.TrimSpace(i.Label) == "" {
			t.Errorf("%s: empty label", name)
		}
		if !cats[i.Category] {
			t.Errorf("%s: category %q is not one the screen lists", name, i.Category)
		}
		if i.Severity == "" || i.Outcome == "" {
			t.Errorf("%s: severity/outcome not set", name)
		}
		if !i.Known {
			t.Errorf("%s: Known is false on a registered action", name)
		}
		// A refusal that reads as routine would never be noticed.
		if i.Outcome != OutcomeOK && i.Severity != SevDanger {
			t.Errorf("%s: outcome %q but severity %q — a failure must read as one", name, i.Outcome, i.Severity)
		}
	}
}

// An action newer than the catalog must still read as words and land somewhere
// sensible, rather than vanish from every category filter.
func TestDescribeFallsBackToTheFamily(t *testing.T) {
	got := Describe("worklog.some_future_thing")
	if got.Known {
		t.Fatalf("an unregistered action reported itself as known")
	}
	if got.Label != "รายการชั่วโมง" || got.Category != CatHours {
		t.Errorf("fallback = %q/%q, want the worklog family", got.Label, got.Category)
	}
	if got := Describe("something.secret.view"); got.Category != CatView {
		t.Errorf("a .view action landed in %q, want the view category", got.Category)
	}
	if got := Describe("auth.magic_failed"); got.Outcome != OutcomeFailed || got.Severity != SevDanger {
		t.Errorf("a _failed action read as %q/%q, want a failure", got.Outcome, got.Severity)
	}
	if got := Describe("zzz.unheard_of"); got.Label != "zzz.unheard_of" {
		t.Errorf("an action with no family should keep its own name, got %q", got.Label)
	}
}

func TestActionsLabelledSearchesTheWordsOnScreen(t *testing.T) {
	got := ActionsLabelled("อนุมัติชั่วโมง")
	found := false
	for _, a := range got {
		if a == "worklog.approve" {
			found = true
		}
	}
	if !found {
		t.Errorf("searching the label did not find worklog.approve: %v", got)
	}
	if got := ActionsLabelled("   "); got != nil {
		t.Errorf("a blank search matched %d actions, want none", len(got))
	}
}

func TestDevice(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36":                   "Chrome บน macOS",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0":           "Edge บน Windows",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1": "Safari บน iPhone",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/126.0 Mobile/15E148 Safari/604.1":  "Chrome บน iPhone",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36":                   "Chrome บน Android",
		"Mozilla/5.0 (X11; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0":                                                                  "Firefox บน Linux",
		"curl/8.6.0": "โปรแกรมอัตโนมัติ (ไม่ใช่เบราว์เซอร์)",
		"":           "",
		"???":        "อุปกรณ์ที่ไม่รู้จัก",
	}
	for ua, want := range cases {
		if got := Device(ua); got != want {
			t.Errorf("Device(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestNetwork(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1":   "local",
		"::1":         "local",
		"10.88.4.21":  "private",
		"192.168.1.9": "private",
		"203.0.113.7": "public",
		"not-an-ip":   "",
		"":            "",
	}
	for ip, want := range cases {
		if got := Network(ip); got != want {
			t.Errorf("Network(%q) = %q, want %q", ip, got, want)
		}
	}
}
