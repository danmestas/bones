package workspace

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const (
	healthzPollInterval = 50 * time.Millisecond
	healthzPollTimeout  = 2 * time.Second
)

// spawnLeaf execs the leaf binary, waits for /healthz to report 200, and
// writes its PID next to the workspace config. Returns the child PID.
// On any failure the child process is killed before returning.
func spawnLeaf(ctx context.Context, p spawnParams) (int, error) {
	logFile, err := os.OpenFile(p.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open leaf log: %w", err)
	}

	cmd := exec.Command(p.LeafBinary,
		"--repo", p.RepoPath,
		"--serve-http", p.HTTPAddr,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return 0, fmt.Errorf("start leaf: %w", err)
	}
	_ = logFile.Close()

	pid := cmd.Process.Pid
	pidPath := filepath.Join(filepath.Dir(p.LogPath), "leaf.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		_ = cmd.Process.Kill()
		return 0, fmt.Errorf("write pid: %w", err)
	}

	healthzURL := "http://" + p.HTTPAddr + "/healthz"
	if err := waitHealthz(ctx, healthzURL); err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(pidPath)
		return 0, err
	}
	return pid, nil
}

// waitHealthz polls the given URL until it returns 200 or the timeout elapses.
// Returns ErrLeafStartTimeout on timeout.
func waitHealthz(ctx context.Context, url string) error {
	deadline := time.Now().Add(healthzPollTimeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	client := &http.Client{Timeout: healthzPollInterval}
	for {
		if time.Now().After(deadline) {
			return ErrLeafStartTimeout
		}
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ErrLeafStartTimeout
		case <-time.After(healthzPollInterval):
		}
	}
}
