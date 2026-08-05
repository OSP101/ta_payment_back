package service

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Notification links are the one string in this service that is only ever
// exercised by a human clicking it. /ta/worklog shipped, pointed at nothing, and
// stayed broken through every rejection anyone ever sent — nothing in Go or in
// the type system connects a link to a Next.js route.
//
// This walks the app router and fails on any literal link the service sends that
// no page can serve.

// Only LITERAL links can be checked: one built by concatenation ("/lecturer/
// courses/"+id+"/reports") has a hole in the middle this cannot resolve. The
// trailing `,` or `(` before the quote is what distinguishes an argument that is
// wholly a string from the tail of an expression.
var notifyLinkRe = regexp.MustCompile(`s\.notify\.Send\((?s:[^()]*?)[,(]\s*"(/[a-z][^"]*)"\s*\)`)

func TestNotificationLinksResolveToRealPages(t *testing.T) {
	root := repoRoot(t)
	appDir := filepath.Join(root, "..", "ta_payment_front", "app")
	if _, err := os.Stat(appDir); err != nil {
		t.Skipf("frontend not checked out beside the backend: %v", err)
	}

	var offenders []string
	err := filepath.Walk(filepath.Join(root, "internal", "service"),
		func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range notifyLinkRe.FindAllStringSubmatch(string(src), -1) {
				if !routeExists(appDir, m[1]) {
					offenders = append(offenders,
						filepath.Base(path)+": "+m[1])
				}
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Errorf("notification links to a page that does not exist — %s", o)
	}
}

// routeExists maps "/ta/courses/x/worklog" onto app/**/page.tsx, treating any
// path segment that came from a Go expression as a dynamic [param] segment.
func routeExists(appDir, link string) bool {
	segs := []string{}
	for _, s := range strings.Split(strings.Trim(link, "/"), "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return findPage(appDir, segs)
}

func findPage(dir string, segs []string) bool {
	if len(segs) == 0 {
		if _, err := os.Stat(filepath.Join(dir, "page.tsx")); err == nil {
			return true
		}
		// Route groups — "(home)" does not appear in the URL.
		return anyGroup(dir, func(sub string) bool { return findPage(sub, nil) })
	}
	// Literal child.
	if _, err := os.Stat(filepath.Join(dir, segs[0])); err == nil {
		if findPage(filepath.Join(dir, segs[0]), segs[1:]) {
			return true
		}
	}
	// Dynamic child, e.g. [tcId].
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "[") {
			if findPage(filepath.Join(dir, e.Name()), segs[1:]) {
				return true
			}
		}
	}
	return anyGroup(dir, func(sub string) bool { return findPage(sub, segs) })
}

func anyGroup(dir string, fn func(string) bool) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "(") {
			if fn(filepath.Join(dir, e.Name())) {
				return true
			}
		}
	}
	return false
}
