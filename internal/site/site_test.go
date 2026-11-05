// Package site checks that the website in docs/ stays true to the code.
package site

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const docs = "../../docs"

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// loaderKeys collects every key the config loader accepts: the serviceKeys
// list, the arguments of every only() call and the top-level keys in
// project()'s switch.
func loaderKeys(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../config/*.go")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	add := func(exprs []ast.Expr) {
		for _, e := range exprs {
			if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, _ := strconv.Unquote(lit.Value)
				keys[s] = true
			}
		}
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				if len(n.Names) == 1 && n.Names[0].Name == "serviceKeys" {
					add(n.Values[0].(*ast.CompositeLit).Elts)
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "only" {
					add(n.Args)
				}
			case *ast.FuncDecl:
				if n.Name.Name == "project" {
					ast.Inspect(n.Body, func(m ast.Node) bool {
						if c, ok := m.(*ast.CaseClause); ok {
							add(c.List)
						}
						return true
					})
				}
			}
			return true
		})
	}
	if len(keys) < 50 {
		t.Fatalf("found only %d loader keys; did the loader's shape change?", len(keys))
	}
	return keys
}

func TestReferenceListsEveryKey(t *testing.T) {
	ref := read(t, filepath.Join(docs, "reference.html"))
	for k := range loaderKeys(t) {
		if !strings.Contains(ref, "<code>"+k+"</code>") {
			t.Errorf("docs/reference.html does not mention <code>%s</code>, which the loader accepts", k)
		}
	}
}

func TestGuideUnitMatchesContrib(t *testing.T) {
	unit := read(t, "../../contrib/anastasis.service")
	if !strings.Contains(read(t, filepath.Join(docs, "guide.html")), unit) {
		t.Error("the unit in docs/guide.html differs from contrib/anastasis.service")
	}
}

var (
	linkAttr = regexp.MustCompile(`(?:href|src)="([^"]*)"`)
	idAttr   = regexp.MustCompile(`id="([^"]*)"`)
)

// TestLinksResolve checks every local href and src: the file exists in
// docs/, and a #fragment names an id in it. 404.html uses /anastasis/ paths,
// because Pages serves it at any depth.
func TestLinksResolve(t *testing.T) {
	pages, err := filepath.Glob(filepath.Join(docs, "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]map[string]bool{}
	for _, p := range pages {
		ids[filepath.Base(p)] = map[string]bool{}
		for _, m := range idAttr.FindAllStringSubmatch(read(t, p), -1) {
			ids[filepath.Base(p)][m[1]] = true
		}
	}
	for _, p := range pages {
		page := filepath.Base(p)
		for _, m := range linkAttr.FindAllStringSubmatch(read(t, p), -1) {
			link := m[1]
			if strings.HasPrefix(link, "https://") {
				continue
			}
			target, frag, _ := strings.Cut(strings.TrimPrefix(link, "/anastasis/"), "#")
			switch target {
			case "":
				target = page
			case "./":
				target = "index.html"
			}
			if _, err := os.Stat(filepath.Join(docs, target)); err != nil {
				t.Errorf("%s: link %q: %v", page, link, err)
				continue
			}
			if frag != "" && !ids[target][frag] {
				t.Errorf("%s: link %q: no id %q in %s", page, link, frag, target)
			}
		}
	}
}
