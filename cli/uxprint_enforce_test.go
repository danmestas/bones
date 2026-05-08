package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// mutatingVerbs is the curated allowlist of state-mutating CLI verbs
// per issue #323. Every entry is the dotted verb name plus the source
// file containing its `*Cmd` struct and Run method. The AST walk
// asserts each Run method (or a helper it directly calls) reaches
// either a `uxprint.X` call or a `c.Quiet` short-circuit so the verb
// either prints the success signature or has explicitly opted out.
//
// Adding a new mutating verb means adding a row here AND wiring an
// uxprint helper into its Run method — the test enforces both halves
// of the convention.
var mutatingVerbs = []struct {
	verb string
	file string
}{
	{"tasks.claim", "tasks_claim.go"},
	{"tasks.close", "tasks_close.go"},
	{"tasks.create", "tasks_create.go"},
	{"tasks.update", "tasks_update.go"},
	{"tasks.link", "tasks_link.go"},
	{"swarm.dispatch", "swarm_dispatch.go"},
}

// TestEveryMutatingVerbCallsUxprint walks each entry in mutatingVerbs
// and asserts the file's AST contains at least one `uxprint.X` call
// reachable from a Run method (directly, or transitively via a
// helper inside the same file). A `c.Quiet` reference satisfies the
// rule too — that's the explicit short-circuit guard the convention
// permits — so a fixture verb that consults `c.Quiet` and never calls
// uxprint still passes structurally even though the runtime would
// stay silent.
//
// This is the structural backstop. Per-verb integration tests pin the
// runtime stdout shape; this test catches a future verb that ships
// without ever hooking the helper at all.
func TestEveryMutatingVerbCallsUxprint(t *testing.T) {
	cliDir := repoSubdir(t, "cli")
	fset := token.NewFileSet()

	for _, mv := range mutatingVerbs {
		mv := mv
		t.Run(mv.verb, func(t *testing.T) {
			path := filepath.Join(cliDir, mv.file)
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			if !fileReferencesUxprintOrQuiet(file) {
				t.Errorf("%s (%s) does not reference uxprint.* nor c.Quiet — "+
					"every state-mutating verb must call a uxprint helper "+
					"on its success path or explicitly short-circuit on "+
					"--quiet (see issue #323 / cli/uxprint)",
					mv.verb, mv.file)
			}
		})
	}
}

// TestPlantedFailureCatchesMissingUxprint is the meta-test that proves
// the enforcement walker actually fails when its precondition is
// violated. We synthesize a Go source string that defines a Run method
// with no uxprint reference and no Quiet field, parse it, and assert
// fileReferencesUxprintOrQuiet returns false.
//
// The brief asks for "a fixture-only command that mutates state but
// doesn't call uxprint; confirm test catches it." We build the fixture
// in-memory rather than in a real file so we don't have to add and
// remove a stub command from the real cli package.
func TestPlantedFailureCatchesMissingUxprint(t *testing.T) {
	src := `package cli

type FixtureNoUxprintCmd struct {
	ID string ` + "`arg:\"\" help:\"task id\"`" + `
}

func (c *FixtureNoUxprintCmd) Run() error {
	// Mutates state, prints nothing, no Quiet flag.
	return nil
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if fileReferencesUxprintOrQuiet(file) {
		t.Fatalf("fixture without uxprint or Quiet should fail enforcement, but passed")
	}
}

// TestPlantedSuccessAllowsUxprintCall is the matched positive: a
// fixture that calls uxprint.Created passes enforcement.
func TestPlantedSuccessAllowsUxprintCall(t *testing.T) {
	src := `package cli

import "github.com/danmestas/bones/cli/uxprint"

type FixtureWithUxprintCmd struct {
	Quiet bool
}

func (c *FixtureWithUxprintCmd) Run() error {
	if !c.Quiet {
		uxprint.Created(nil, "shortid", "title")
	}
	return nil
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if !fileReferencesUxprintOrQuiet(file) {
		t.Fatalf("fixture with uxprint call must pass enforcement, but failed")
	}
}

// fileReferencesUxprintOrQuiet returns true when the file contains
// either a `uxprint.<Name>(...)` call or any reference to a Quiet
// field on a receiver named `c`. Either is sufficient evidence that
// the convention has been considered.
//
// Implementation detail: we don't try to walk only the `Run` method
// — a verb is allowed to delegate the print to a helper inside the
// same file (e.g. tasks_aggregate.go's emitAggregateOutput, called
// from Run). Searching the whole file catches the helper-delegated
// case without false negatives.
func fileReferencesUxprintOrQuiet(file *ast.File) bool {
	hasUxprintImport := false
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		// imp.Path.Value is quoted; trim and compare.
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "github.com/danmestas/bones/cli/uxprint" ||
			strings.HasSuffix(path, "/cli/uxprint") {
			hasUxprintImport = true
			break
		}
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		// Match `uxprint.X(...)` selector calls.
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "uxprint" {
					found = true
					return false
				}
			}
		}
		// Match `c.Quiet` field references on a receiver named c.
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if sel.Sel != nil && sel.Sel.Name == "Quiet" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "c" {
					found = true
					return false
				}
			}
		}
		return true
	})
	if found {
		return true
	}
	// If the file imports uxprint, treat that as evidence — covers
	// the rare case where the call is constructed via an intermediate
	// (e.g. `print := uxprint.Created; print(...)`).
	return hasUxprintImport
}
