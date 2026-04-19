// Package coord is the single public entry point for agent-infra.
//
// Agents construct a Coord via Open, call methods to Claim file-scoped
// work, list Ready tasks, CloseTask, Post chat messages, Ask synchronous
// questions, and Close the Coord at shutdown. Substrate details —
// NATS subjects, fossil transactions, hold encodings — live below this
// boundary in internal packages and never appear in these signatures.
//
// See docs/invariants.md for the runtime invariants every public method
// enforces. See docs/adr/ for the architectural commitments that shaped
// this surface (ADR 0001 public surface, 0002 scoped holds, 0003
// substrate hiding, 0004 conflict resolution).
package coord
