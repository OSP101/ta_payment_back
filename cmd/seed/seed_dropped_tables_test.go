package main

import (
	"os"
	"regexp"
	"testing"
)

// The seed ignores Exec errors, so a write into a table a migration has since
// dropped fails silently forever (budget_caps, dropped in 0114, sat here for
// months). Pin the known-dropped tables out of the seed.
func TestSeedDoesNotWriteDroppedTables(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"budget_caps"} {
		if regexp.MustCompile(`(?i)INSERT\s+INTO\s+` + table + `\b`).Match(src) {
			t.Errorf("seed still inserts into %s, which migrations dropped", table)
		}
	}
}
