package cli

import (
	"time"
)

// timeNow is the time source for swarm verbs. Plain wrapper today;
// pulled into a function so future test-time injection (e.g. a fixed
// clock in unit tests) is a one-line change rather than a refactor.
func timeNow() time.Time {
	return time.Now().UTC()
}
