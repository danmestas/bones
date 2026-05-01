package registry

import (
	"net/http"
	"os"
	"syscall"
	"time"
)

var HealthTimeout = 500 * time.Millisecond

// IsAlive returns true if BOTH (a) the recorded HubPID is alive on this host
// AND (b) GET <HubURL>/health returns 200 within HealthTimeout. Both checks are
// required because a recycled PID can pass (a) but fail (b).
func IsAlive(e Entry) bool {
	if !pidAlive(e.HubPID) {
		return false
	}
	client := &http.Client{Timeout: HealthTimeout}
	resp, err := client.Get(e.HubURL + "/health")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
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
