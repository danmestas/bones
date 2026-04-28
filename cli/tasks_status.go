package cli

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/tasks"
)

// TasksStatusCmd prints a one-shot snapshot of hub and backlog state.
//
// Output:
//
//	hub:      http://127.0.0.1:8765 (pid 12345)
//	nats:     nats://127.0.0.1:4222
//	backlog:  3 open · 1 claimed · 2 closed (last 24h)
type TasksStatusCmd struct{}

func (c *TasksStatusCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	// Hub liveness: read the PID file.
	pidPath := filepath.Join(info.WorkspaceDir, ".bones", "leaf.pid")
	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		fmt.Fprintln(os.Stderr,
			"hub not running — run `bash .orchestrator/scripts/hub-bootstrap.sh`\n"+
				"  (or `bones init` to create a fresh workspace)")
		return fmt.Errorf("read pid: %w", err)
	}
	pid := strings.TrimSpace(string(pidStr))

	// Quick health-check: GET /healthz on the leaf HTTP endpoint.
	healthy := leafHealthy(info.LeafHTTPURL)
	hubLine := fmt.Sprintf("%s (pid %s)", info.LeafHTTPURL, pid)
	if !healthy {
		hubLine += " [UNREACHABLE]"
	}

	// Backlog counts via the tasks manager.
	nc, err := nats.Connect(info.NATSURL)
	if err != nil {
		fmt.Printf("hub:     %s\nnats:    %s [UNREACHABLE]\n",
			hubLine, info.NATSURL)
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()

	m, err := tasks.Open(ctx, nc, tasks.Config{
		BucketName:   tasks.DefaultBucketName,
		HistoryDepth: 10,
		MaxValueSize: 64 * 1024,
		ChanBuffer:   32,
	})
	if err != nil {
		fmt.Printf("hub:     %s\nnats:    %s\n", hubLine, info.NATSURL)
		return fmt.Errorf("tasks.Open: %w", err)
	}
	defer func() { _ = m.Close() }()

	all, err := m.List(ctx)
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}

	var open, claimed, closed24h int
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, t := range all {
		switch t.Status {
		case tasks.StatusOpen:
			open++
		case tasks.StatusClaimed:
			claimed++
		case tasks.StatusClosed:
			if t.ClosedAt != nil && t.ClosedAt.After(cutoff) {
				closed24h++
			}
		}
	}

	fmt.Printf("hub:     %s\n", hubLine)
	fmt.Printf("nats:    %s\n", info.NATSURL)
	fmt.Printf("backlog: %d open · %d claimed · %d closed (last 24h)\n",
		open, claimed, closed24h)
	return nil
}

// leafHealthy returns true if the leaf's /healthz endpoint responds with 200
// within a short timeout.
func leafHealthy(baseURL string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(baseURL + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
