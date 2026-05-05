package cli

import (
	"testing"

	"github.com/alecthomas/kong"

	"github.com/danmestas/bones/internal/tasks"
)

// These tests pin issue #213's regression surface: --parent must remain a
// first-class Kong flag on `bones tasks create` and `bones tasks update`,
// and must route to the structural tasks.Task.Parent field — not into the
// free-form Context metadata map. Triage of #213 found the original
// reproduction (`unknown flag --parent` on bones 0.8.0) does not occur on
// current main; --parent has been declared since the typed-Kong port. This
// file locks that contract so a future refactor can't silently regress it.

// TestTasksCreateCmd_ParsesParentFlag asserts Kong recognizes --parent on
// `bones tasks create` and stores the value on the structural field
// (TasksCreateCmd.Parent), not anywhere in the --context-derived metadata
// payload.
func TestTasksCreateCmd_ParsesParentFlag(t *testing.T) {
	var c TasksCreateCmd
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"some-title", "--parent", "ROOT-1"}); err != nil {
		t.Fatalf("parse --parent: %v", err)
	}
	if c.Parent != "ROOT-1" {
		t.Fatalf("Parent=%q, want %q", c.Parent, "ROOT-1")
	}
	// Critical: --parent must NOT round-trip through --context. If a
	// future refactor decides to "unify" parent linkage by stuffing it
	// into c.Context (e.g. as "parent=ROOT-1"), this guard fails.
	for _, kv := range c.Context {
		if len(kv) >= 7 && kv[:7] == "parent=" {
			t.Errorf("--parent leaked into --context payload: %q", kv)
		}
	}
}

// TestTasksCreateCmd_ParentBuildsStructuralTask mirrors the relevant slice
// of TasksCreateCmd.Run: with --parent X and --context k=v, the resulting
// tasks.Task has Parent=X (structural) and Context only contains the
// caller-supplied k=v (no shadow "parent" key).
func TestTasksCreateCmd_ParentBuildsStructuralTask(t *testing.T) {
	var c TasksCreateCmd
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	args := []string{
		"child-title",
		"--parent", "PARENT-XYZ",
		"--context", "k=v",
	}
	if _, err := parser.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Replicate the structural assignment from TasksCreateCmd.Run so the
	// regression test exercises the exact wiring that ships in the CLI.
	ctxMap := applyContext(nil, c.Context)
	built := tasks.Task{
		Title:   c.Title,
		Parent:  c.Parent,
		Context: ctxMap,
	}

	if built.Parent != "PARENT-XYZ" {
		t.Errorf("built.Parent=%q, want PARENT-XYZ", built.Parent)
	}
	if got, ok := built.Context["parent"]; ok {
		t.Errorf(
			"Context[\"parent\"]=%q present; "+
				"--parent must use structural field, not metadata",
			got,
		)
	}
	if got := built.Context["k"]; got != "v" {
		t.Errorf("Context[\"k\"]=%q, want v (unrelated --context pair must still apply)", got)
	}
}

// TestTasksUpdateCmd_ParsesParentFlag asserts Kong parses --parent on
// `bones tasks update` into the pointer-typed Parent field. The pointer
// type (rather than plain string) is load-bearing: it lets the mutator
// distinguish "flag absent" from "flag set to empty string" so an explicit
// --parent "" can clear an existing parent linkage.
func TestTasksUpdateCmd_ParsesParentFlag(t *testing.T) {
	var c TasksUpdateCmd
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"task-id-1", "--parent", "NEW-PARENT"}); err != nil {
		t.Fatalf("parse --parent: %v", err)
	}
	if c.Parent == nil {
		t.Fatalf("Parent pointer nil; flag was provided")
	}
	if *c.Parent != "NEW-PARENT" {
		t.Fatalf("*Parent=%q, want NEW-PARENT", *c.Parent)
	}
}

// TestTasksUpdateCmd_ParentMutatorSets exercises buildUpdateMutator: when
// --parent is supplied, the closure rewrites the structural Parent field
// on the input task. This is the actual write path that mgr.Update invokes
// inside TasksUpdateCmd.Run.
func TestTasksUpdateCmd_ParentMutatorSets(t *testing.T) {
	newParent := "NEW-PARENT-ID"
	c := &TasksUpdateCmd{Parent: &newParent}
	var out tasks.Task
	mut := buildUpdateMutator(c, "", nil, &out)

	in := tasks.Task{ID: "child", Parent: "OLD-PARENT", Status: tasks.StatusOpen}
	got, err := mut(in)
	if err != nil {
		t.Fatalf("mutator: %v", err)
	}
	if got.Parent != newParent {
		t.Errorf("Parent=%q, want %q", got.Parent, newParent)
	}
	if out.Parent != newParent {
		t.Errorf("captured out.Parent=%q, want %q", out.Parent, newParent)
	}
	// Sanity: Context map is untouched by --parent.
	if _, ok := got.Context["parent"]; ok {
		t.Errorf("mutator leaked Parent into Context map")
	}
}

// TestTasksUpdateCmd_ParentMutatorClears pins the clear-semantics: invoking
// `bones tasks update <id> --parent ""` is the documented way to detach a
// task from its parent. The pointer-typed flag plus the parentSet branch
// in buildUpdateMutator together make this explicit.
func TestTasksUpdateCmd_ParentMutatorClears(t *testing.T) {
	empty := ""
	c := &TasksUpdateCmd{Parent: &empty}
	var out tasks.Task
	mut := buildUpdateMutator(c, "", nil, &out)

	in := tasks.Task{ID: "child", Parent: "OLD-PARENT", Status: tasks.StatusOpen}
	got, err := mut(in)
	if err != nil {
		t.Fatalf("mutator: %v", err)
	}
	if got.Parent != "" {
		t.Errorf("Parent=%q, want empty (cleared)", got.Parent)
	}
}

// TestTasksUpdateCmd_ParentMutatorAbsentPreserves pins the inverse contract:
// when --parent is NOT supplied, the mutator must leave the existing Parent
// untouched. This is the entire reason TasksUpdateCmd.Parent is *string and
// not string — without the pointer, "absent" and "explicit empty" collapse.
func TestTasksUpdateCmd_ParentMutatorAbsentPreserves(t *testing.T) {
	c := &TasksUpdateCmd{Parent: nil}
	var out tasks.Task
	mut := buildUpdateMutator(c, "", nil, &out)

	in := tasks.Task{ID: "child", Parent: "PRESERVED", Status: tasks.StatusOpen}
	got, err := mut(in)
	if err != nil {
		t.Fatalf("mutator: %v", err)
	}
	if got.Parent != "PRESERVED" {
		t.Errorf("Parent=%q, want PRESERVED (untouched)", got.Parent)
	}
}
