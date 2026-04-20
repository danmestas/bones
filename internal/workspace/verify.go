package workspace

import (
	"net/http"
	"os"
	"syscall"
	"time"
)

// pidAlive returns true if a process with the given PID exists and accepts signal 0.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal(0) doesn't deliver a signal; it only checks existence/permissions.
	return proc.Signal(syscall.Signal(0)) == nil
}

// healthzOK returns true if the given URL returns HTTP 200 within the timeout.
func healthzOK(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == 200
}
