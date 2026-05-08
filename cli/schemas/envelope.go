// Package schemas defines the typed payload structs for every
// `--json`-emitting bones CLI verb (ADR 0053). Each verb has one Go
// struct here; the `cmd/bones-schemagen` binary reflects these structs
// and writes derived JSON Schema files into the repo-root `schemas/`
// directory.
//
// Go types in this package are the source of truth for the external
// `--json` contract. Types here are intentionally separate from the
// internal storage types under `internal/tasks`, `internal/swarm`,
// etc.; the duplication pins the external surface so internal types
// can evolve freely behind it.
//
// The envelope below is universal: every emitter wraps its payload
// in `Envelope[T]` so consumers can write one parser that handles
// every verb. The single carve-out (`bones logs --json`, which
// passthrough-emits substrate event-log NDJSON) is documented in
// ADR 0053 and uses the substrate's own contract per ADR 0052.
package schemas

import (
	"encoding/json"
	"fmt"
	"io"
)

// Schema is the meta-block stamped on every bones `--json` emit.
// Verb is the dotted CLI command path (e.g. "tasks.list",
// "swarm.dispatch"). Version is "v" followed by an integer
// (e.g. "v1"); the ADR 0053 hard-cut migration policy means a single
// version of bones emits exactly one version per verb.
type Schema struct {
	Verb    string `json:"verb"`
	Version string `json:"version"`
}

// Envelope wraps every `--json` payload. T is the verb-specific
// typed payload struct defined elsewhere in this package.
//
// Generic over T so the compiler enforces that the wrapped payload
// matches what the schemagen generator reflected for the named
// verb; a copy/paste mismatch (wrapping a TasksListPayload under a
// schema verb of "tasks.show") becomes a build error.
type Envelope[T any] struct {
	Schema Schema `json:"schema"`
	Data   T      `json:"data"`
}

// New builds an Envelope[T] with the given verb name, version
// string, and typed payload. Used by every emitter so the schema
// block is constructed in one place — no emitter spells out the
// verb-name or version-tag literal twice.
func New[T any](verb, version string, data T) Envelope[T] {
	return Envelope[T]{
		Schema: Schema{Verb: verb, Version: version},
		Data:   data,
	}
}

// Emit marshals env as compact JSON to w with a trailing newline.
// Mirrors the pre-existing `emitJSON` helper's wire shape so a
// `--json`-emitting verb wrapped by this helper byte-for-byte
// matches the pre-envelope shape (modulo the wrap).
//
// Compact output is the bones default; emitters that previously
// used indented output (`json.NewEncoder.SetIndent`) keep their
// indented shape via [EmitIndent]. A future v2 may unify these,
// but v1 pins today's per-verb shape.
func Emit[T any](w io.Writer, env Envelope[T]) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write envelope: %w", err)
	}
	return nil
}

// EmitIndent marshals env as indented (2-space) JSON to w with a
// trailing newline emitted by the encoder. Used by verbs whose
// pre-envelope shape was `json.NewEncoder.SetIndent("","  ")` —
// `swarm.status`, `workspaces.list`, `workspaces.get` — so v1
// preserves their human-readable indentation.
func EmitIndent[T any](w io.Writer, env Envelope[T]) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	return nil
}
