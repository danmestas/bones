package holds

import (
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/bones/internal/wspath"
)

// casRetryHook is called once per CAS retry in Announce. Production
// code leaves it as a no-op; tests overwrite it via
// SetCASRetryHookForTest to count retries deterministically without
// mocking the KV bucket. Keeping the hook out of the hot path — one
// indirect call per conflict, never on the fast path — is worth the
// observability. The declaration lives in testhooks.go so the no-op
// default and its Set*ForTest mutator are co-located, but the call
// site is in production Announce (internal/holds/holds.go).
var casRetryHook = func() {}

// KVForTest returns the underlying JetStream KV handle so tests in
// sibling packages can stage CAS-conflict scenarios by writing
// directly to the bucket. Production code must not use this; the
// public API (Announce, Release, WhoHas, Subscribe) remains the sole
// supported surface. The export is intentionally verbose ("ForTest")
// so every call site reads as a test seam, not an accidental leak of
// the substrate.
func (m *Manager) KVForTest() jetstream.KeyValue {
	return m.kv
}

// SetCASRetryHookForTest installs fn as the per-retry hook used by
// Announce's CAS loop, returning a restore function the test must call
// (typically via t.Cleanup) to reinstate the no-op default. Tests use
// this to observe the number of CAS retries without instrumenting the
// KV transport. The hook fires exactly once per retry — never on the
// first attempt or after a final verdict.
func SetCASRetryHookForTest(fn func()) (restore func()) {
	prev := casRetryHook
	casRetryHook = fn
	return func() { casRetryHook = prev }
}

// EncodeForTest exposes the package's internal hold encoder so sibling
// test packages can produce wire-compatible bytes to Put directly into
// the KV bucket. It is not part of the supported public API.
func EncodeForTest(h Hold) ([]byte, error) {
	return encode(h)
}

// KeyForTest exposes the package's file-to-KV-key transform so
// sibling test packages can stage KV entries under the exact key
// Announce uses. Not part of the supported public API.
func KeyForTest(file wspath.Path) string {
	return keyOf(file)
}
