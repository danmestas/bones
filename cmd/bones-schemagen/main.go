// Command bones-schemagen reflects the typed payload structs in
// cli/schemas and emits one JSON Schema file per verb under the
// configured output directory (default: schemas/ at the repo root).
//
// The generator is the source-of-truth pipeline for ADR 0053. CI
// gates drift between the reflected output and the checked-in files
// via `make schemas-check`.
//
// Invocation:
//
//	go run ./cmd/bones-schemagen -out ./schemas
//
// Or, equivalently, via `go generate ./cli/schemas/...`.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/invopop/jsonschema"

	"github.com/danmestas/bones/cli/schemas"
)

func main() {
	out := flag.String("out", "schemas", "output directory for *.json schemas")
	flag.Parse()

	if err := run(*out); err != nil {
		fmt.Fprintf(os.Stderr, "bones-schemagen: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable seam: walks schemas.Verbs, reflects each
// payload type, and writes <verb>.<version>.json under outDir.
// Idempotent — re-running on a clean tree yields no diffs.
func run(outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	// Build a name → payload-instance map by reflecting the schemas
	// package. Every entry in schemas.Verbs must resolve here; an
	// unmapped name is a coding error caught at generation time.
	payloads := payloadInstances()

	for _, v := range schemas.Verbs {
		inst, ok := payloads[v.PayloadName]
		if !ok {
			return fmt.Errorf("verb %q: payload type %q not found in schemas package",
				v.Verb, v.PayloadName)
		}

		ref := &jsonschema.Reflector{
			// Allow additional properties so a future v2 can add
			// fields without invalidating consumer-side validation
			// inside one major version (we hard-cut on bump anyway,
			// but consumers writing strict validators still see a
			// clean v1).
			AllowAdditionalProperties: true,
			// Inline definitions: emitting one self-contained schema
			// per verb is friendlier for downstream tools than
			// $ref-chained docs.
			DoNotReference: true,
			// Anonymous structs produce stable output — no random
			// suffix on their generated names.
			Anonymous: true,
		}
		schema := ref.Reflect(inst)
		schema.Title = v.PayloadName
		schema.Description = fmt.Sprintf(
			"Payload (`data` field) for `bones %s --json` envelope, version %s.",
			verbToCommand(v.Verb), v.CurrentVersion)
		// Pin the JSON Schema dialect explicitly so future
		// invopop/jsonschema upgrades can't shift the spec out from
		// under us silently.
		schema.Version = "https://json-schema.org/draft/2020-12/schema"

		data, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal %s: %w", v.Verb, err)
		}
		data = append(data, '\n')

		path := filepath.Join(outDir,
			fmt.Sprintf("%s.%s.json", v.Verb, v.CurrentVersion))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// payloadInstances returns a name → zero-value-struct map for every
// payload type referenced from schemas.Verbs. Hand-maintained so the
// generator does not need to grow into a Go AST walker; CI gates
// completeness via the per-verb roundtrip tests.
//
// Type aliases (TasksClaimPayload = Task) are listed by alias name
// here so each verb gets its own schema file even when multiple
// verbs share the underlying Go type.
func payloadInstances() map[string]any {
	return map[string]any{
		"DoctorAllPayload":      &schemas.DoctorAllPayload{},
		"StatusAllPayload":      &schemas.StatusAllPayload{},
		"SwarmDispatchPayload":  &schemas.SwarmDispatchPayload{},
		"SwarmStatusPayload":    &schemas.SwarmStatusPayload{},
		"SwarmTasksPayload":     &schemas.SwarmTasksPayload{},
		"TasksAggregatePayload": &schemas.TasksAggregatePayload{},
		"TasksBySlotPayload":    &schemas.TasksBySlotPayload{},
		"TasksClaimPayload":     &schemas.TasksClaimPayload{},
		"TasksClosePayload":     &schemas.TasksClosePayload{},
		"TasksCreatePayload":    &schemas.TasksCreatePayload{},
		"TasksLinkPayload":      &schemas.TasksLinkPayload{},
		"TasksListPayload":      &schemas.TasksListPayload{},
		"TasksPrimePayload":     &schemas.TasksPrimePayload{},
		"TasksReadyPayload":     &schemas.TasksReadyPayload{},
		"TasksShowPayload":      &schemas.TasksShowPayload{},
		"TasksUpdatePayload":    &schemas.TasksUpdatePayload{},
		"WorkspacesGetPayload":  &schemas.WorkspacesGetPayload{},
		"WorkspacesListPayload": &schemas.WorkspacesListPayload{},
	}
}

// verbToCommand maps a dotted verb name back to the human-readable
// CLI command path used in schema descriptions. `tasks.list` →
// `tasks list`, `swarm.dispatch` → `swarm dispatch`.
func verbToCommand(verb string) string {
	out := make([]rune, 0, len(verb))
	for _, r := range verb {
		if r == '.' {
			out = append(out, ' ')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
