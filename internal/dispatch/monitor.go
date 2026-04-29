package dispatch

import (
	"context"
	"fmt"
	"time"
)

// WaitWorkerAbsent polls the substrate's presence view via probe and
// returns nil once workerAgentID is no longer present, or an error
// if the deadline elapses first. Used by the dispatch parent to
// detect worker dropout (heartbeat lapse) before reclaiming the
// claim.
//
// The probe callback decouples dispatch from coord: tests can pass
// a closure backed by an in-memory list, and production wiring uses
// `coord.Coord.PresentAgentIDs`.
func WaitWorkerAbsent(
	ctx context.Context,
	probe PresenceProbe,
	workerAgentID string,
	deadline time.Duration,
) error {
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			return fmt.Errorf(
				"agent %s still present after %s",
				workerAgentID, deadline,
			)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			ids, err := probe(ctx)
			if err != nil {
				return fmt.Errorf("presence probe: %w", err)
			}
			found := false
			for _, id := range ids {
				if id == workerAgentID {
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
