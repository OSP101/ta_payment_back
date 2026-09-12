package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// QUAL-01: `for rows.Next()` loops whose function never calls `rows.Err()`
// silently turn "the iteration stopped because the connection dropped /
// the statement timed out / the context was cancelled" into "the iteration
// stopped because we read every row" — rows.Next() returns false for both,
// and only rows.Err() tells them apart. This test parses every non-test file
// in this package and fails if any `for <ident>.Next()` loop's enclosing
// function never calls `<ident>.Err()` — the AST-level version of the "grep
// for the pattern" step the audit did by hand, run automatically so a new
// occurrence cannot get merged without a matching Err() check.
//
// A whitelist exists for the handful of loops that are provably not
// pgx.Rows (e.g. iterating a Go slice/map that happens to have Next/Err
// methods of its own) — see rowsErrLintAllow below. Add to it only with a
// comment explaining why the loop is not a database row iteration.
func TestNoRowsIterationWithoutErrCheck(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var violations []string

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			// Every `for <ident>.Next()` loop condition inside this function,
			// keyed by the receiver identifier.
			nextIdents := map[string]int{} // ident -> line of the for-loop
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				forStmt, ok := n.(*ast.ForStmt)
				if !ok || forStmt.Cond == nil {
					return true
				}
				call, ok := forStmt.Cond.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Next" || len(call.Args) != 0 {
					return true
				}
				recv, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				nextIdents[recv.Name] = fset.Position(forStmt.Pos()).Line
				return true
			})
			if len(nextIdents) == 0 {
				continue
			}

			// Every `<ident>.Err()` call anywhere in the function.
			errIdents := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Err" || len(call.Args) != 0 {
					return true
				}
				recv, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				errIdents[recv.Name] = true
				return true
			})

			for ident, line := range nextIdents {
				if errIdents[ident] {
					continue
				}
				key := name + ":" + fn.Name.Name + ":" + ident
				if rowsErrLintAllow[key] {
					continue
				}
				violations = append(violations, fmtViolation(name, line, fn.Name.Name, ident))
			}
		}
	}

	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("%d rows.Next() loop(s) with no matching rows.Err() check:\n%s",
			len(violations), strings.Join(violations, "\n"))
	}
}

func fmtViolation(file string, line int, fn, ident string) string {
	return file + ":" + strconv.Itoa(line) + " func " + fn + "(): loops on `" + ident + ".Next()` but never calls `" + ident + ".Err()`"
}

// rowsErrLintAllow keyed by "file.go:FuncName:ident" — loops confirmed NOT to
// be an un-checked pgx.Rows iteration. Empty on purpose: every real
// occurrence found by this test during QUAL-01 was a genuine gap and got a
// rows.Err() check instead of an entry here.
var rowsErrLintAllow = map[string]bool{}
