package dispatch

import (
	"context"
	"fmt"
	"time"

	"github.com/danmestas/bones/coord"
)

func WaitWorkerAbsent(
	ctx context.Context,
	c *coord.Coord,
	workerAgentID string,
	deadline time.Duration,
) error {
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			return fmt.Errorf("agent %s still present after %s", workerAgentID, deadline)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			entries, err := c.Who(ctx)
			if err != nil {
				return fmt.Errorf("who: %w", err)
			}
			found := false
			for _, p := range entries {
				if p.AgentID() == workerAgentID {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
	}
}

func ReclaimClaim(
	ctx context.Context,
	c *coord.Coord,
	taskID coord.TaskID,
	ttl time.Duration,
) (func() error, error) {
	return c.Reclaim(ctx, taskID, ttl)
}
