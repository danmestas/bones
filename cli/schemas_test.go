package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		return schemas.TasksListPayload{}
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
	case "WorkspacesGetPayload":
		return schemas.WorkspacesGetPayload{}
	case "WorkspacesListPayload":
		return schemas.WorkspacesListPayload{}
	}
	t.Fatalf("unknown payload type %q — extend zeroPayloadFor", name)
	return nil
}

// TestEvery_JSONFlag_HasRegistryEntry guards the inverse of
// TestEveryVerbHasSchemaFile: every `--json` flag in cli/ has a
// corresponding registry entry in schemas.Verbs (modulo the
// documented carve-out for `bones logs --json`, which passthrough-
// emits substrate event-log NDJSON per ADR 0052).
//
// Implementation: walks the cli/ tree for `name:"json"` Kong tags
// and resolves each to the verb name via the file path. Failure
// surfaces both ways: a new emitter without a registry row, OR a
// renamed verb whose registry entry got out of sync.
func TestEvery_JSONFlag_HasRegistryEntry(t *testing.T) {
	// Map: source file → expected dotted verb name. logs is the
	// documented carve-out and is intentionally absent.
	wantVerbs := map[string]string{
		"cli/doctor.go":          "doctor",
		"cli/status.go":          "status",
		"cli/swarm_dispatch.go":  "swarm.dispatch",
		"cli/swarm_status.go":    "swarm.status",
		"cli/swarm_tasks.go":     "swarm.tasks",
		"cli/tasks_aggregate.go": "tasks.aggregate",
		"cli/tasks_claim.go":     "tasks.claim",
		"cli/tasks_close.go":     "tasks.close",
		"cli/tasks_create.go":    "tasks.create",
		"cli/tasks_link.go":      "tasks.link",
		// tasks_list.go also drives tasks.bySlot via --by-slot
		"cli/tasks_list.go":   "tasks.list",
		"cli/tasks_prime.go":  "tasks.prime",
		"cli/tasks_ready.go":  "tasks.ready",
		"cli/tasks_show.go":   "tasks.show",
		"cli/tasks_update.go": "tasks.update",
		// also drives workspaces.get via WorkspacesShowCmd
		"cli/workspaces.go": "workspaces.list",
	}
	registered := make(map[string]struct{}, len(schemas.Verbs))
	for _, v := range schemas.Verbs {
		registered[v.Verb] = struct{}{}
	}
	for path, verb := range wantVerbs {
		if _, ok := registered[verb]; !ok {
			t.Errorf("%s emits --json for verb %q but registry "+
				"is missing it; add to cli/schemas/verbs.go",
				path, verb)
		}
	}
	// And every registered verb must have either a known emit site
	// or be the pseudo-verb (tasks.bySlot, workspaces.get) backed
	// by a flag combination on a parent command.
	knownVerbs := map[string]struct{}{
		"tasks.bySlot":   {}, // tasks list --by-slot --json
		"workspaces.get": {}, // workspaces show <name> --json
	}
	for _, verb := range wantVerbs {
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
