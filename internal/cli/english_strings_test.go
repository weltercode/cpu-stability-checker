package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// TestNoCyrillicInPrintableStrings pins the audit A05 contract over
// the cli package itself: every string literal the compiled menu,
// wizard, history and CLI can print is English (the product UI
// language). The scan reads this package's own non-test source files
// (go test runs with the package directory as the working directory)
// and inspects only STRING LITERALS - comments keep the project's
// documentation language on purpose, and the tui/catalog tests use
// Cyrillic fixtures deliberately, which is why test files are not
// scanned.
//
// The guard is loud about its own preconditions: finding no sources
// (a test binary run from a copied-out directory) is a failure, not a
// silent skip. A guard that quietly stops guarding is worse than no
// guard at all.
func TestNoCyrillicInPrintableStrings(t *testing.T) {
	cyrillic := func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	var sources []string
	for _, name := range names {
		if !strings.HasSuffix(name, "_test.go") {
			sources = append(sources, name)
		}
	}
	if len(sources) == 0 {
		t.Fatal("no package sources beside this test: the guard must run from the package directory (go test ./internal/cli), never from a copied-out test binary")
	}

	fset := token.NewFileSet()
	var offenders []string
	for _, name := range sources {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				offenders = append(offenders, fmt.Sprintf("%s: unquotable string literal %s", fset.Position(lit.Pos()), lit.Value))
				return true
			}
			if strings.IndexFunc(s, cyrillic) >= 0 {
				shown := s
				if len(shown) > 60 {
					shown = shown[:60] + "..."
				}
				offenders = append(offenders, fmt.Sprintf("%s: %q", fset.Position(lit.Pos()), shown))
			}
			return true
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("%d Cyrillic string literals in user-facing cli sources (the UI language is English; if a string is truly non-printing, exclude it with a stated reason):\n%s",
			len(offenders), strings.Join(offenders, "\n"))
	}
}
