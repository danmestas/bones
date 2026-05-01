package registry

import (
	"net/http"
	"os"
	"syscall"
	"time"
)

var HealthTimeout = 500 * time.Millisecond

// IsAlive returns true if BOTH (a) the recorded HubPID is alive on this host
// AND (b) GET <HubURL> succeeds at the TCP/HTTP level within HealthTimeout.
// Both checks are required because a recycled PID can pass (a) but fail (b).
//
// The HTTP probe doesn't require any specific endpoint — any HTTP response
// (including 4xx) means the port is bound and serving, which is what we
// actually want to know. The Fossil HTTP server bones uses doesn't expose a
// /health endpoint and we deliberately don't add a sidecar HTTP server just
// for the probe.
func IsAlive(e Entry) bool {
	if !pidAlive(e.HubPID) {
		return false
	}
	client := &http.Client{Timeout: HealthTimeout}
	resp, err := client.Get(e.HubURL)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return true // any HTTP response means port is bound and serving
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
