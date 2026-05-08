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

// timeTimeJSONFieldAllowlist names files that contain bare time.Time
// json-tagged struct fields and have NOT migrated to LoggedTime.
// Every entry is a deferred-sweep target documenting why the
// migration didn't land in #324.
//
// The #324 brief named exactly three migration targets:
// cli/schemas/types.go, cli/schemas/verbs.go, and the
// EventEnvelope.Timestamp field in internal/tasks/events.go. Files
// below carry bare time.Time fields too, but the migration there
// involves additional substrate concerns (KV record format, in-flight
// stream messages, cross-version compatibility) that are larger than
// the single-PR scope and deserve their own follow-ups.
//
// Allowlist categories:
//
//   - internal/tasks/task.go: the internal Task struct round-trips
//     through JSON inside CreatedPayload.Snapshot and KV records.
//     Sweep changes JetStream wire format — separate refactor.
//
//   - internal/swarm/{events,session}.go: live swarm session
//     records on the JetStream KV. Cross-version compatibility
//     with running leaves rules out a same-PR sweep.
//
//   - internal/registry/{info,registry}.go: registry on disk.
//     Cross-version compatibility with workspaces written by
//     older bones binaries; sweep wants a one-shot migration tool.
//
//   - internal/sessions/sessions.go, internal/holds/hold.go,
//     internal/presence/entry.go, internal/dispatch/manifest.go,
//     internal/chat/chat.go, internal/updatecheck/updatecheck.go,
//     internal/logwriter/events.go: internal-substrate types whose
//     JSON shape is not part of the public --json contract. Sweep
//     is mechanical but out of #324's named scope; left to a
//     follow-up so the diff stays focused.
//
// Adding NEW entries to this list requires a follow-up issue
// documenting the migration plan; reviewers should reject growth
// without one.
var timeTimeJSONFieldAllowlist = map[string]bool{
	filepath.Join("internal", "chat", "chat.go"):               true,
	filepath.Join("internal", "dispatch", "manifest.go"):       true,
	filepath.Join("internal", "holds", "hold.go"):              true,
	filepath.Join("internal", "logwriter", "events.go"):        true,
	filepath.Join("internal", "presence", "entry.go"):          true,
	filepath.Join("internal", "registry", "info.go"):           true,
	filepath.Join("internal", "registry", "registry.go"):       true,
	filepath.Join("internal", "sessions", "sessions.go"):       true,
	filepath.Join("internal", "swarm", "events.go"):            true,
	filepath.Join("internal", "swarm", "session.go"):           true,
	filepath.Join("internal", "tasks", "task.go"):              true,
	filepath.Join("internal", "updatecheck", "updatecheck.go"): true,
}

// TestNoBareTimeTimeJSONFields walks the bones source tree and
// fails if any struct field declares time.Time (or *time.Time) with
// a json tag outside the allowlist above. JSON-marshaled time.Time
// emits RFC3339Nano in the local zone with an offset suffix —
// directly violating the Logged policy. Every payload struct must
// declare timefmt.LoggedTime instead.
//
// # Heuristic
//
// We walk struct field declarations: if the field type is time.Time
// or *time.Time AND the field has a struct tag containing "json:",
// it's flagged. False positives outside cli/schemas are unlikely;
// the allowlist absorbs the only deferred-sweep target.
//
// # Why this complements the .Format walker
//
// The .Format walker catches direct callsites — it cannot see the
// reflective marshal path that encoding/json takes through a
// time.Time field. Together, the two tests close both holes
// (direct Format calls + json marshal path).
func TestNoBareTimeTimeJSONFields(t *testing.T) {
	repoRoot := findRepoRoot(t)
	roots := []string{"cli", "cmd", "internal"}

	var violations []string
	for _, root := range roots {
		root := filepath.Join(repoRoot, root)
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
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
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			if timeTimeJSONFieldAllowlist[rel] {
				return nil
			}
			hits, err := scanBareTimeJSONFields(path)
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
		t.Fatalf("found bare time.Time json-tagged struct fields — "+
			"every payload timestamp must use timefmt.LoggedTime "+
			"per #324 so the JSON marshal path emits UTC RFC3339:"+
			"\n  %s", strings.Join(violations, "\n  "))
	}
}

// scanBareTimeJSONFields parses the file at path and returns every
// struct field whose type is time.Time or *time.Time AND whose tag
// contains "json:".
func scanBareTimeJSONFields(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	var hits []string
	ast.Inspect(file, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, f := range st.Fields.List {
			if f.Tag == nil {
				continue
			}
			tag := f.Tag.Value
			if !strings.Contains(tag, "json:") {
				continue
			}
			if !isTimeTimeType(f.Type) {
				continue
			}
			pos := fset.Position(f.Pos())
			names := "<embedded>"
			if len(f.Names) > 0 {
				ns := make([]string, len(f.Names))
				for i, n := range f.Names {
					ns[i] = n.Name
				}
				names = strings.Join(ns, ",")
			}
			hits = append(hits, pos.String()+": field "+names+
				" has type time.Time with json tag")
		}
		return true
	})
	return hits, nil
}

// isTimeTimeType reports whether expr names time.Time or *time.Time.
// Doesn't recurse into named-type aliases — bare time.Time and the
// pointer form are the only shapes the JSON marshaler treats as
// "use the default time.Time MarshalJSON path", which is the shape
// we want to forbid.
func isTimeTimeType(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		if !ok {
			return false
		}
		return id.Name == "time" && e.Sel != nil && e.Sel.Name == "Time"
	case *ast.StarExpr:
		return isTimeTimeType(e.X)
	}
	return false
}

// TestPlantedFailureCatchesBareTimeJSONField confirms the field
// scanner flags a fixture struct with a time.Time + json tag.
func TestPlantedFailureCatchesBareTimeJSONField(t *testing.T) {
	tmp := t.TempDir()
	fixture := filepath.Join(tmp, "bad.go")
	body := `package bad

import "time"

type Payload struct {
	At time.Time ` + "`json:\"at\"`" + `
}
`
	if err := os.WriteFile(fixture, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	hits, err := scanBareTimeJSONFields(fixture)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("scanner missed bare time.Time json field — " +
			"heuristic regression would let payload structs " +
			"bypass LoggedTime")
	}
}

// TestPlantedSuccessAllowsLoggedTimeField confirms the field
// scanner does NOT flag a struct that uses timefmt.LoggedTime.
func TestPlantedSuccessAllowsLoggedTimeField(t *testing.T) {
	tmp := t.TempDir()
	fixture := filepath.Join(tmp, "good.go")
	body := `package good

import "github.com/danmestas/bones/internal/timefmt"

type Payload struct {
	At timefmt.LoggedTime ` + "`json:\"at\"`" + `
}
`
	if err := os.WriteFile(fixture, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	hits, err := scanBareTimeJSONFields(fixture)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) > 0 {
		t.Fatalf("scanner false-positive on LoggedTime field: %v", hits)
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
