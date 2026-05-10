package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/danmestas/bones/cli/schemas"
)

// TestEveryVerbHasSchemaFile pins ADR 0053 acceptance criterion #4:
// the generator's checked-in artifact set covers every verb in the
// registry. A new verb appended to schemas.Verbs that hasn't run
// `make schemas` yet trips here, mirroring the make schemas-check
// gate at the unit-test layer.
func TestEveryVerbHasSchemaFile(t *testing.T) {
	for _, v := range schemas.Verbs {
		path := schemaPath(t, v.Verb, v.CurrentVersion)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("verb %q (%s): missing schema file %s — run `make schemas`",
				v.Verb, v.CurrentVersion, path)
		}
	}
}

// TestEnvelopeRoundtripPerVerb walks the registry and asserts each
// verb's payload struct round-trips through the envelope helpers
// without losing fields. Constructs a zero-value payload, wraps,
// emits, parses back via Envelope[json.RawMessage] (so we don't
// have to spell out 17 generic instantiations), and validates the
// envelope's wire shape and the `data` block against the
// checked-in JSON Schema file.
func TestEnvelopeRoundtripPerVerb(t *testing.T) {
	for _, v := range schemas.Verbs {
		v := v
		t.Run(v.Verb, func(t *testing.T) {
			payload := zeroPayloadFor(t, v.PayloadName)
			env := schemas.New(v.Verb, v.CurrentVersion, payload)

			var buf bytes.Buffer
			if err := schemas.Emit(&buf, env); err != nil {
				t.Fatalf("Emit: %v", err)
			}

			// Parse the wire bytes back: schema block first, then
			// data as raw bytes for schema-side validation.
			var got schemas.Envelope[json.RawMessage]
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal envelope: %v\npayload=%s", err, buf.String())
			}
			if got.Schema.Verb != v.Verb {
				t.Errorf("schema.verb = %q, want %q", got.Schema.Verb, v.Verb)
			}
			if got.Schema.Version != v.CurrentVersion {
				t.Errorf("schema.version = %q, want %q",
					got.Schema.Version, v.CurrentVersion)
			}

			validateAgainstSchema(t, v.Verb, v.CurrentVersion, got.Data)
		})
	}
}

// TestSchemaFilesParseable asserts every checked-in schema file is
// valid JSON Schema 2020-12. A hand-edit (forbidden by ADR 0053)
// that produces invalid JSON Schema would trip here even when
// schemas-check happens to compare equal byte-for-byte.
func TestSchemaFilesParseable(t *testing.T) {
	for _, v := range schemas.Verbs {
		v := v
		t.Run(v.Verb, func(t *testing.T) {
			path := schemaPath(t, v.Verb, v.CurrentVersion)
			compileSchemaFile(t, path)
		})
	}
}

// schemaPath resolves <repo-root>/schemas/<verb>.<version>.json.
// Walks up from the test working directory (./cli) to find the
// repo root by the presence of go.mod.
func schemaPath(t *testing.T, verb, version string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "schemas",
				fmt.Sprintf("%s.%s.json", verb, version))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repo root from %s", dir)
		}
		dir = parent
	}
}

// validateAgainstSchema compiles the checked-in schema for verb
// and validates raw against it. Decoding raw into `any` first is
// required by santhosh-tekuri/jsonschema's value contract.
func validateAgainstSchema(t *testing.T, verb, version string, raw json.RawMessage) {
	t.Helper()
	compiled := compileSchemaFile(t, schemaPath(t, verb, version))
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode data block: %v", err)
	}
	if err := compiled.Validate(doc); err != nil {
		t.Errorf("data does not validate against %s.%s.json:\n%v",
			verb, version, err)
	}
}

// compileSchemaFile loads + compiles one JSON Schema. Returns the
// compiled schema; failure is t.Fatal because every verb's schema
// is supposed to be a buildable artifact.
func compileSchemaFile(t *testing.T, path string) *jsonschema.Schema {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema %s: %v", path, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse schema %s: %v", path, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(path, doc); err != nil {
		t.Fatalf("add schema resource %s: %v", path, err)
	}
	compiled, err := c.Compile(path)
	if err != nil {
		t.Fatalf("compile schema %s: %v", path, err)
	}
	return compiled
}

// zeroPayloadFor returns a zero-valued payload struct for a given
// schemas.Verbs entry. The mapping mirrors cmd/bones-schemagen's
// payloadInstances — duplicated here on purpose so a future
// generator-side refactor doesn't silently skip a verb in the
// roundtrip test.
//
// Slice and map fields are pre-initialized to non-nil zero values
// so JSON marshaling emits `[]` / `{}` instead of `null`, matching
// the runtime emitter behavior (every list-shaped payload's empty
// case still serializes as an array).
func zeroPayloadFor(t *testing.T, name string) any {
	t.Helper()
	switch name {
	case "DoctorAllPayload":
		return schemas.DoctorAllPayload{
			Workspaces: []schemas.DoctorWorkspaceRow{},
		}
	case "StatusAllPayload":
		return schemas.StatusAllPayload{
			Workspaces: []schemas.StatusWorkspaceRow{},
		}
	case "SwarmDispatchPayload":
		return schemas.SwarmDispatchPayload{}
	case "SwarmStatusPayload":
		return schemas.SwarmStatusPayload{}
	case "SwarmTasksPayload":
		return schemas.SwarmTasksPayload{}
	case "TasksAggregatePayload":
		return schemas.TasksAggregatePayload{
			Slots: []schemas.TasksAggregateSlot{},
		}
	case "TasksBySlotPayload":
		return schemas.TasksBySlotPayload{
			Slots: []schemas.TasksSlotGroup{},
		}
	case "TasksClaimPayload":
		return schemas.TasksClaimPayload{Files: []string{}}
	case "TasksClosePayload":
		return schemas.TasksClosePayload{Files: []string{}}
	case "TasksCreatePayload":
		return schemas.TasksCreatePayload{Files: []string{}}
	case "TasksLinkPayload":
		return schemas.TasksLinkPayload{}
	case "TasksListPayload":
		return schemas.TasksListPayload{Tasks: []schemas.Task{}}
	case "TasksPrimePayload":
		return schemas.TasksPrimePayload{
			OpenTasks:    []schemas.TasksPrimeTask{},
			ReadyTasks:   []schemas.TasksPrimeTask{},
			ClaimedTasks: []schemas.TasksPrimeTask{},
			Threads:      []schemas.TasksPrimeThread{},
			Peers:        []schemas.TasksPrimePresence{},
		}
	case "TasksReadyPayload":
		return schemas.TasksReadyPayload{}
	case "TasksShowPayload":
		return schemas.TasksShowPayload{Files: []string{}}
	case "TasksUpdatePayload":
		return schemas.TasksUpdatePayload{Files: []string{}}
	case "UpPayload":
		return schemas.UpPayload{
			Actions: []schemas.UpAction{},
		}
	case "WorkspacesGetPayload":
		return schemas.WorkspacesGetPayload{}
	case "WorkspacesListPayload":
		return schemas.WorkspacesListPayload{}
	}
	t.Fatalf("unknown payload type %q — extend zeroPayloadFor", name)
	return nil
}

// TestEvery_JSONFlag_HasRegistryEntry guards the inverse of
// TestEveryVerbHasSchemaFile: every `--json` flag declared via a
// Kong tag in cli/ must map to a known emitter, and the registry
// must cover every emitter.
//
// Implementation: AST-walks every cli/*.go (non-test) file, finds
// every struct field whose tag contains `name:"json"`, and resolves
// the file path to its expected dotted verb name via [fileToVerb].
// Self-maintaining: a new --json emitter without a fileToVerb
// mapping or registry entry trips this test.
//
// The single intentional exemption is `cli/logs.go` — `bones logs
// --json` passthrough-emits substrate event-log NDJSON per ADR
// 0052 and is the documented carve-out from the envelope.
func TestEvery_JSONFlag_HasRegistryEntry(t *testing.T) {
	cliDir := repoSubdir(t, "cli")
	emitters := scanJSONFlagEmitters(t, cliDir)

	if len(emitters) == 0 {
		t.Fatalf("AST walk found zero --json emitters in %s — walker broken",
			cliDir)
	}

	// Map every source file to the dotted verb its --json flag
	// produces. Files not in the map either don't emit JSON
	// (vacuously true) or are documented carve-outs (logs.go).
	fileToVerb := map[string]string{
		"doctor.go":          "doctor",
		"status.go":          "status",
		"swarm_dispatch.go":  "swarm.dispatch",
		"swarm_status.go":    "swarm.status",
		"swarm_tasks.go":     "swarm.tasks",
		"tasks_aggregate.go": "tasks.aggregate",
		"tasks_claim.go":     "tasks.claim",
		"tasks_close.go":     "tasks.close",
		"tasks_create.go":    "tasks.create",
		"tasks_link.go":      "tasks.link",
		// tasks_list.go also drives tasks.bySlot via --by-slot;
		// the flag is shared so we map to the primary verb here.
		"tasks_list.go":   "tasks.list",
		"tasks_prime.go":  "tasks.prime",
		"tasks_ready.go":  "tasks.ready",
		"tasks_show.go":   "tasks.show",
		"tasks_update.go": "tasks.update",
		// init.go declares UpCmd; --json on `bones up` lives there
		// per the existing struct layout (#314).
		"init.go": "up",
		// workspaces.go also drives workspaces.get via WorkspacesShowCmd.
		"workspaces.go": "workspaces.list",
	}
	exempt := map[string]bool{
		"logs.go": true, // ADR 0053 / ADR 0052 carve-out
		// tasks_watch.go --json emits streaming NDJSON
		// (one EventEnvelope per line) per ADR 0052; the
		// registry holds one-shot envelope verbs only. Same
		// carve-out shape as logs.go.
		"tasks_watch.go": true,
	}

	registered := make(map[string]struct{}, len(schemas.Verbs))
	for _, v := range schemas.Verbs {
		registered[v.Verb] = struct{}{}
	}

	// Forward direction: every emitter site must resolve to a
	// known verb that lives in the registry.
	for _, file := range emitters {
		if exempt[file] {
			continue
		}
		verb, ok := fileToVerb[file]
		if !ok {
			t.Errorf("%s declares a --json flag but is not in the "+
				"fileToVerb map; either map it to its dotted verb "+
				"or add it to the exempt list with an ADR-cited reason",
				file)
			continue
		}
		if _, ok := registered[verb]; !ok {
			t.Errorf("%s emits --json for verb %q but registry "+
				"is missing it; add to cli/schemas/verbs.go",
				file, verb)
		}
	}

	// Reverse direction: every registry entry must be covered by
	// a known emitter (either a real --json flag or a documented
	// pseudo-verb backed by a flag combination on a parent command).
	knownVerbs := map[string]struct{}{
		"tasks.bySlot":   {}, // tasks list --by-slot --json
		"workspaces.get": {}, // workspaces show <name> --json
	}
	for _, verb := range fileToVerb {
		knownVerbs[verb] = struct{}{}
	}
	for v := range registered {
		if _, ok := knownVerbs[v]; !ok {
			t.Errorf("registry entry %q has no documented emit site; "+
				"either remove from registry or document the verb "+
				"in TestEvery_JSONFlag_HasRegistryEntry", v)
		}
	}
}

// repoSubdir resolves <repo-root>/<sub> by walking up from the
// test working directory to find go.mod. Mirrors [schemaPath] but
// returns a directory instead of a file path.
func repoSubdir(t *testing.T, sub string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, sub)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repo root from %s", dir)
		}
		dir = parent
	}
}

// scanJSONFlagEmitters parses every non-test Go file under dir and
// returns the basenames whose top-level command struct declares a
// field with `name:"json"` in its Kong tag. AST-walks rather than
// greps so a future tag-syntax tweak (spaces, single quotes) keeps
// matching accurately.
func scanJSONFlagEmitters(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var hits []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		if hasJSONFlagField(file) {
			hits = append(hits, name)
		}
	}
	return hits
}

// hasJSONFlagField reports whether file contains any struct field
// whose Kong tag has `name:"json"`. Walks every StructType node so
// nested or sibling-command structs are caught.
func hasJSONFlagField(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, f := range st.Fields.List {
			if f.Tag == nil {
				continue
			}
			// f.Tag.Value is the source-level back-tick literal
			// including the surrounding quotes; trim them so
			// reflect.StructTag parses it cleanly.
			tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
			if tag.Get("name") == "json" {
				found = true
				return false
			}
		}
		return true
	})
	return found
}
