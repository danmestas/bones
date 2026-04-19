package holds

import (
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

// isCASConflict reports whether err is a JetStream KV revision-guard
// rejection — either a Create on a key that already exists or an
// Update whose expected-last-sequence did not match the current
// sequence. Both surface as the server API error code
// JSErrCodeStreamWrongLastSequence (10071); ErrKeyExists carries that
// same code, so errors.Is covers the Update path too via the
// jsError/APIError unwrap chain. We additionally compare the raw
// APIError code so a future library change that ungroups the two
// sentinels won't silently turn CAS conflicts into "unknown error".
func isCASConflict(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, jetstream.ErrKeyExists) {
		return true
	}
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		if apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence {
			return true
		}
	}
	return false
}

// casRetryHook is called once per CAS retry in Announce. Production
// code leaves it as a no-op; tests overwrite it via kvForTest to count
// retries deterministically without mocking the KV bucket. Keeping the
// hook out of the hot path — one indirect call per conflict, never on
// the fast path — is worth the observability.
var casRetryHook = func() {}
