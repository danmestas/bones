package workspace

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
		"--nats-client-port", strconv.Itoa(p.NATSClientPort),
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
		if tail := logTail(p.LogPath, 4096); tail != "" {
			return 0, fmt.Errorf("%w; leaf log:\n%s", err, tail)
		}
		return 0, err
	}
	return pid, nil
}

func logTail(path string, maxBytes int) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	if len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
	}
	return strings.TrimSpace(string(data))
}

// waitHealthz polls the given URL until it returns 200 or the timeout elapses.
// Returns ErrLeafStartTimeout on timeout.
func waitHealthz(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, healthzPollTimeout)
	defer cancel()

	client := &http.Client{Timeout: healthzPollInterval}
	ticker := time.NewTicker(healthzPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ErrLeafStartTimeout
		case <-ticker.C:
			resp, err := client.Get(url)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					return nil
				}
			}
		}
	}
}
