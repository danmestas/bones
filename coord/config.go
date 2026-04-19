package coord

import (
	"fmt"
	"time"
)

// Config is the operator-supplied configuration for a Coord instance.
// Every field is required; there are no silent defaults. Validate
// enforces bounded limits per the Phase 1 TigerStyle commitment.
type Config struct {
	// AgentID identifies this coord instance across the substrate.
	AgentID string

	// HoldTTLDefault is the default TTL callers pass to Claim when they
	// do not have a TTL of their own.
	HoldTTLDefault time.Duration

	// HoldTTLMax is the upper bound on any Claim's TTL. Enforced at
	// Claim entry via assertion per invariant 5.
	HoldTTLMax time.Duration

	// MaxHoldsPerClaim caps the file count on a single Claim call per
	// invariant 4.
	MaxHoldsPerClaim int

	// MaxSubscribers caps the number of in-flight chat subscribers.
	MaxSubscribers int

	// MaxTaskFiles caps the number of files a single task may touch.
	MaxTaskFiles int

	// OperationTimeout bounds a single coord operation end-to-end.
	OperationTimeout time.Duration

	// HeartbeatInterval is the cadence at which coord refreshes its
	// liveness signal to the substrate.
	HeartbeatInterval time.Duration

	// NATSReconnectWait is the delay between NATS reconnection attempts.
	NATSReconnectWait time.Duration

	// NATSMaxReconnects caps the number of NATS reconnection attempts
	// before Open returns an error or a live Coord surfaces a terminal
	// disconnect.
	NATSMaxReconnects int

	// NATSURL is the URL coord.Open dials to reach the substrate. It
	// never appears in any coord public method signature per ADR 0003;
	// it lives on Config because it is operator-supplied input.
	NATSURL string
}

// Validate checks every Config field against its documented bounds and
// returns the first violation as an error. The error message follows
// the shape "coord.Config: <field>: <reason>". Validate is pure; it
// does not panic on bad operator input per invariant 9.
func (c Config) Validate() error {
	if c.AgentID == "" {
		return fmt.Errorf("coord.Config: AgentID: must be non-empty")
	}
	if c.HoldTTLDefault <= 0 {
		return fmt.Errorf("coord.Config: HoldTTLDefault: must be > 0")
	}
	if c.HoldTTLMax <= 0 {
		return fmt.Errorf("coord.Config: HoldTTLMax: must be > 0")
	}
	if c.HoldTTLDefault > c.HoldTTLMax {
		return fmt.Errorf(
			"coord.Config: HoldTTLDefault: must be <= HoldTTLMax",
		)
	}
	if c.MaxHoldsPerClaim <= 0 {
		return fmt.Errorf("coord.Config: MaxHoldsPerClaim: must be > 0")
	}
	if c.MaxSubscribers <= 0 {
		return fmt.Errorf("coord.Config: MaxSubscribers: must be > 0")
	}
	if c.MaxTaskFiles <= 0 {
		return fmt.Errorf("coord.Config: MaxTaskFiles: must be > 0")
	}
	if c.OperationTimeout <= 0 {
		return fmt.Errorf("coord.Config: OperationTimeout: must be > 0")
	}
	if c.HeartbeatInterval <= 0 {
		return fmt.Errorf("coord.Config: HeartbeatInterval: must be > 0")
	}
	if c.NATSReconnectWait <= 0 {
		return fmt.Errorf("coord.Config: NATSReconnectWait: must be > 0")
	}
	if c.NATSMaxReconnects <= 0 {
		return fmt.Errorf("coord.Config: NATSMaxReconnects: must be > 0")
	}
	if c.NATSURL == "" {
		return fmt.Errorf("coord.Config: NATSURL: must be non-empty")
	}
	return nil
}
