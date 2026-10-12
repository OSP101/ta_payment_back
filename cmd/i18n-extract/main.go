// Command i18n-extract lists the Thai strings in the API that can reach a
// reader's screen — error messages and in-app notification text — so they can
// be translated into internal/i18n/catalog_en.json.
//
//	go run ./cmd/i18n-extract > /tmp/keys.json          every candidate
//	go run ./cmd/i18n-extract -missing > /tmp/todo.json  only those not yet in the catalog
//
// Packages that only produce official documents or e-mail stay Thai on
// purpose and are skipped (pdfgen, docxgen, mail, watermark). In the export
// services only strings passed to an error constructor are taken: the rest is
// spreadsheet/document content.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var thai = regexp.MustCompile(`[\x{0E00}-\x{0E7F}]`)

var skipDirs = map[string]bool{"pdfgen": true, "docxgen": true, "mail": true, "watermark": true, "testutil": true}

// Files whose non-error strings are document content.
var docFile = regexp.MustCompile(`(export|xlsx|appointment_order|timetable_form|_mail|grad_lump_ledger|grad_special_schedule|data_export)`)

var errFuncs = map[string]bool{"NewError": true, "Invalid": true, "Conflict": true, "Forbidden": true, "New": true, "Errorf": true, "NotFoundf": true}

type entry struct {
	Key   string   `json:"key"`
	Where []string `json:"where"`
}

func main() {
	missing := flag.Bool("missing", false, "only keys not yet in internal/i18n/catalog_en.json")
	flag.Parse()
	found := map[string]map[string]bool{}
	fset := token.NewFileSet()
	_ = filepath.Walk("internal", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		errorsOnly := docFile.MatchString(filepath.Base(path))
		inErr := map[*ast.BasicLit]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			if errFuncs[name] {
				for _, a := range call.Args {
					ast.Inspect(a, func(m ast.Node) bool {
						if bl, ok := m.(*ast.BasicLit); ok {
							inErr[bl] = true
						}
						return true
					})
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(bl.Value)
			if err != nil || !thai.MatchString(s) {
				return true
			}
			if errorsOnly && !inErr[bl] {
				return true
			}
			s = strings.TrimSpace(s)
			if found[s] == nil {
				found[s] = map[string]bool{}
			}
			found[s][fmt.Sprintf("%s:%d", path, fset.Position(bl.Pos()).Line)] = true
			return true
		})
		return nil
	})
	have := map[string]string{}
	if *missing {
		if b, err := os.ReadFile("internal/i18n/catalog_en.json"); err == nil {
			_ = json.Unmarshal(b, &have)
		}
	}
	var out []entry
	for k, w := range found {
		if *missing && have[k] != "" {
			continue
		}
		e := entry{Key: k}
		for p := range w {
			e.Where = append(e.Where, p)
		}
		sort.Strings(e.Where)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Where[0] < out[j].Where[0] })
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	_ = enc.Encode(out)
}
