package timefmt_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenTimeFormatCallsites walks the bones source tree and
// fails if any non-test file outside internal/timefmt/ calls
// time.Time.Format directly. Every timestamp surface must route through
// timefmt.Logged or timefmt.Display so the per-surface zone policy
// (#324) is enforceable rather than aspirational.
//
// # Heuristic
//
// We don't use go/types for full type inference (that would require
// loading the whole module's type graph for every CI run). Instead we
// flag any expression of the shape <expr>.Format(<args>) in a non-test
// file outside the helper package. False positives — e.g.
// fmt.Stringer-like custom Format methods on non-time types — are
// caught by an allowlist below. The allowlist is intentionally tiny:
// if it has to grow, the heuristic is wrong and we should switch to
// go/types instead.
//
// The selector that follows .Format must look like a time-format
// argument to count: a quoted layout string or a time.RFC3339-family
// constant. Anything else (e.g. fmt.Sprintf("%s", x.Format(y, z))
// where Format takes multiple args) is presumed not to be
// time.Time.Format.
//
// # Limits
//
//   - Variables holding a layout string defeat the heuristic.
//     Acceptable: the convention is "no time.Format anywhere outside
//     the helper", so a future contributor inventing such a pattern
//     would fail review on grounds the test missed.
//   - Reflective/indirect calls (e.g. via interface satisfaction)
//     defeat the heuristic. Acceptable: the same review backstop
//     applies.
func TestNoForbiddenTimeFormatCallsites(t *testing.T) {
	repoRoot := findRepoRoot(t)

	// Roots we walk. CLI/binaries/internal — skip vendored deps,
	// .worktrees scratch dirs, and tooling.
	roots := []string{"cli", "cmd", "internal"}

	var violations []string

	for _, root := range roots {
		root := filepath.Join(repoRoot, root)
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				// Skip the timefmt package itself — that's the one
				// place direct .Format calls are allowed.
				if filepath.Base(path) == "timefmt" {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			hits, err := scanForbiddenFormat(path)
			if err != nil {
				return err
			}
			violations = append(violations, hits...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(violations) > 0 {
		t.Fatalf("found forbidden time.Format callsites — every "+
			"timestamp surface must route through internal/timefmt "+
			"per #324:\n  %s", strings.Join(violations, "\n  "))
	}
}

// TestPlantedFailureCatchesForbiddenFormat is the meta-test: confirm
// that the scanner does flag a fixture file containing an exact
// `t.Format(time.RFC3339)` call. Without this, a regression in the
// scanner could let real violations slip past while the main test
// stays silently green.
func TestPlantedFailureCatchesForbiddenFormat(t *testing.T) {
	tmp := t.TempDir()
	fixture := filepath.Join(tmp, "bad.go")
	body := `package bad

import "time"

func emit(t time.Time) string {
	return t.Format(time.RFC3339)
}
`
	if err := os.WriteFile(fixture, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	hits, err := scanForbiddenFormat(fixture)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("scanner missed t.Format(time.RFC3339) — heuristic " +
			"regression would let real violations through")
	}
}

// TestPlantedSuccessAllowsHelperCall confirms the scanner does NOT
// flag a file that uses timefmt.Logged. This is the matched positive
// to the planted-failure test — together they pin the scanner's
// shape.
func TestPlantedSuccessAllowsHelperCall(t *testing.T) {
	tmp := t.TempDir()
	fixture := filepath.Join(tmp, "good.go")
	body := `package good

import (
	"time"

	"github.com/danmestas/bones/internal/timefmt"
)

func emit(t time.Time) string {
	_ = time.Now()
	return timefmt.Logged(t)
}
`
	if err := os.WriteFile(fixture, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	hits, err := scanForbiddenFormat(fixture)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("scanner false-positive on helper call: %v", hits)
	}
}

// scanForbiddenFormat parses the file at path and returns every
// "<expr>.Format(<layout>)" callsite where the layout argument
// looks like a time-format. The returned strings are
// "<path>:<line>: <expr>.Format(...)" so the test failure points at
// the offender directly.
func scanForbiddenFormat(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	var hits []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel == nil || sel.Sel.Name != "Format" {
			return true
		}
		// .Format takes exactly one argument when it's time.Time.Format
		// (the layout string). fmt.Sprintf, fmt.Formatter.Format, and
		// io/fs.File.Format-style methods all take other shapes.
		if len(call.Args) != 1 {
			return true
		}
		if !looksLikeTimeLayout(call.Args[0]) {
			return true
		}
		pos := fset.Position(call.Pos())
		hits = append(hits, formatHit(pos, sel))
		return true
	})
	return hits, nil
}

// looksLikeTimeLayout returns true when arg is recognizably a
// time-format layout: either a string literal containing the
// reference date components (HH:MM:SS or 2006-01-02 or the year
// 2006), or a selector expression on the time package
// (time.RFC3339, time.RFC3339Nano, time.Kitchen, etc.).
func looksLikeTimeLayout(arg ast.Expr) bool {
	switch a := arg.(type) {
	case *ast.BasicLit:
		// String literal — check for reference-date markers.
		v := strings.Trim(a.Value, "`\"")
		return strings.Contains(v, "15:04") ||
			strings.Contains(v, "2006") ||
			strings.Contains(v, "Jan _2") ||
			strings.Contains(v, "Mon Jan")
	case *ast.SelectorExpr:
		// time.RFC3339, time.RFC3339Nano, time.Kitchen, time.ANSIC, etc.
		id, ok := a.X.(*ast.Ident)
		if !ok {
			return false
		}
		if id.Name != "time" {
			return false
		}
		name := a.Sel.Name
		// Allowlist of layout constants exported from the time package.
		switch name {
		case "RFC3339", "RFC3339Nano", "RFC1123", "RFC1123Z", "RFC822",
			"RFC822Z", "RFC850", "ANSIC", "UnixDate", "RubyDate",
			"Kitchen", "Stamp", "StampMilli", "StampMicro", "StampNano",
			"DateTime", "DateOnly", "TimeOnly", "Layout":
			return true
		}
		return false
	}
	return false
}

// formatHit pretty-prints one violation site. Receiver-side text is
// best-effort: complex expressions render as "<expr>" rather than
// the literal Go source.
func formatHit(pos token.Position, sel *ast.SelectorExpr) string {
	recv := exprText(sel.X)
	return strings.TrimSpace(
		pos.String() + ": " + recv + ".Format(...)",
	)
}

// exprText renders a small subset of expressions back to
// human-readable text. Good enough for diagnostic messages.
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprText(v.Fun) + "(...)"
	}
	return "<expr>"
}

// findRepoRoot walks up from the test working directory to find a
// directory containing go.mod. Mirrors cli/schemas_test.go's
// repoSubdir helper but lives here so the timefmt package's tests
// don't import cli.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repo root from %s", dir)
		}
		dir = parent
	}
}
