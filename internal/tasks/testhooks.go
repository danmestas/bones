package tasks

import "github.com/nats-io/nats.go/jetstream"

// KVForTest returns the underlying JetStream KV handle so tests in
// sibling packages can stage CAS-conflict scenarios by writing directly
// to the bucket. Production code must not use this; the public API
// (Open, Close, Create, Get, Update, List, Watch) remains the sole
// supported surface. The export is intentionally verbose ("ForTest")
// so every call site reads as a test seam, not an accidental leak of
// the substrate.
func (m *Manager) KVForTest() jetstream.KeyValue {
	return m.kv
}

// SetCASRetryHookForTest installs fn as the per-retry hook used by
// Update's CAS loop, returning a restore function the test must call
// (typically via t.Cleanup) to reinstate the no-op default. Tests use
// this to observe the number of CAS retries without instrumenting the
// KV transport. The hook fires exactly once per retry — never on the
// first attempt or after a final verdict.
func SetCASRetryHookForTest(fn func()) (restore func()) {
	prev := casRetryHook
	casRetryHook = fn
	return func() { casRetryHook = prev }
}

// SetUpdatePreWriteHookForTest installs fn as the per-attempt hook used
// by Update's CAS loop between the Get and the Update calls. Tests use
// this seam to deterministically force CAS conflicts by performing a
// direct Put while fn runs — every attempt reliably fails under such a
// hook, making the retry-exhaustion path reachable in a single test.
// Restore must be called (typically via t.Cleanup) to reinstate the
// no-op default.
func SetUpdatePreWriteHookForTest(fn func()) (restore func()) {
	prev := updatePreWriteHook
	updatePreWriteHook = fn
	return func() { updatePreWriteHook = prev }
}

// EncodeForTest exposes the package's internal task encoder so sibling
// test packages can produce wire-compatible bytes to Put directly into
// the KV bucket. Not part of the supported public API.
func EncodeForTest(t Task) ([]byte, error) {
	return encode(t)
}
