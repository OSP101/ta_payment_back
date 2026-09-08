package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// QUAL-04: cover_image_key checked its prefix but not "..", and storage.Local
// itself had no traversal guard of its own — every caller had to remember to
// check ".." before calling Open, which is exactly the kind of rule a new
// caller (an email renderer, an export, a thumbnailer) forgets. This pins
// the guard at the store itself: a key that walks outside root must be
// refused, not read.
func TestLocal_OpenRefusesPathTraversal(t *testing.T) {
	root := t.TempDir()
	st, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}

	// A real secret file OUTSIDE root that a traversal would reach.
	secretDir := t.TempDir()
	secretPath := filepath.Join(secretDir, "passwd")
	if err := os.WriteFile(secretPath, []byte("root:x:0:0"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Craft a key that walks from root back up to secretDir.
	rel, err := filepath.Rel(root, secretPath)
	if err != nil {
		t.Fatal(err)
	}
	traversalKey := filepath.ToSlash(rel)

	if _, err := st.Open(traversalKey); err == nil {
		t.Fatalf("Open(%q) succeeded — traversal escaped root", traversalKey)
	}
	if err := st.Delete(traversalKey); err == nil {
		t.Fatalf("Delete(%q) succeeded — traversal escaped root", traversalKey)
	}

	// A literal "announcements/../../../etc/passwd"-shaped key, the concrete
	// case QUAL-04 names.
	if _, err := st.Open("announcements/../../../etc/passwd"); err == nil {
		t.Fatal(`Open("announcements/../../../etc/passwd") succeeded — traversal escaped root`)
	}
}

// A key that stays inside root, however deep, must keep working — the guard
// must not be so strict it breaks normal Save/Open round trips.
func TestLocal_ResolveAllowsOrdinaryKeys(t *testing.T) {
	root := t.TempDir()
	st, err := NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := st.Save("ta_docs", "id.pdf", strings.NewReader("%PDF-1.7\n"))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := st.Open(key)
	if err != nil {
		t.Fatalf("Open of a normally-saved key must succeed: %v", err)
	}
	rc.Close()
}
