package coord

import (
	"fmt"
	"time"
)

// TuningConfig holds substrate knobs that almost no real caller varies.
// Zero value in any field means "use sane defaults" — Open applies
// defaultTuning before Validate so callers only set what they need.
type TuningConfig struct {
	// HoldTTLDefault is the default TTL callers pass to Claim when they
	// do not supply a TTL of their own. Default: 30s.
	HoldTTLDefault time.Duration

	// HoldTTLMax is the upper bound on any Claim's TTL. Default: 5m.
	HoldTTLMax time.Duration

	// MaxHoldsPerClaim caps the file count on a single Claim call per
	// invariant 4. Default: 16.
	MaxHoldsPerClaim int

	// MaxSubscribers caps the number of in-flight chat subscribers.
	// Default: 8.
	MaxSubscribers int

	// MaxTaskFiles caps the number of files a single task may touch.
	// Default: 16.
	MaxTaskFiles int

	// MaxReadyReturn caps the number of tasks coord.Ready returns in a
	// single call. Default: 32.
	MaxReadyReturn int

	// MaxTaskValueSize is the upper bound on a task record's serialized
	// JSON value, in bytes. Default: 16384.
	MaxTaskValueSize int

	// TaskHistoryDepth is the per-key JetStream KV history depth for the
	// tasks bucket. Default: 8.
	TaskHistoryDepth uint8

	// HeartbeatInterval is the cadence at which coord refreshes its
	// liveness signal. Default: 5s.
	HeartbeatInterval time.Duration

	// NATSReconnectWait is the delay between NATS reconnection attempts.
	// Default: 100ms.
	NATSReconnectWait time.Duration

	// NATSMaxReconnects caps NATS reconnection attempts. Default: 10.
	NATSMaxReconnects int

	// Buggify is a test-only flag; zero in production.
	Buggify int
}

// defaultTuning returns a TuningConfig with sane production defaults.
// Only zero fields are filled in — callers that set a field explicitly
// keep their value.
func defaultTuning(t TuningConfig) TuningConfig {
	if t.HoldTTLDefault == 0 {
		t.HoldTTLDefault = 30 * time.Second
	}
	if t.HoldTTLMax == 0 {
		t.HoldTTLMax = 5 * time.Minute
	}
	if t.MaxHoldsPerClaim == 0 {
		t.MaxHoldsPerClaim = 16
	}
	if t.MaxSubscribers == 0 {
		t.MaxSubscribers = 8
	}
	if t.MaxTaskFiles == 0 {
		t.MaxTaskFiles = 16
	}
	if t.MaxReadyReturn == 0 {
		t.MaxReadyReturn = 32
	}
	if t.MaxTaskValueSize == 0 {
		t.MaxTaskValueSize = 16384
	}
	if t.TaskHistoryDepth == 0 {
		t.TaskHistoryDepth = 8
	}
	if t.HeartbeatInterval == 0 {
		t.HeartbeatInterval = 5 * time.Second
	}
	if t.NATSReconnectWait == 0 {
		t.NATSReconnectWait = 100 * time.Millisecond
	}
	if t.NATSMaxReconnects == 0 {
		t.NATSMaxReconnects = 10
	}
	return t
}

// Config is the operator-supplied configuration for a Coord instance.
// Only the four identity/routing fields are required; Tuning is
// zero-safe — Open fills missing fields from defaultTuning.
type Config struct {
	// AgentID identifies this coord instance across the substrate.
	AgentID string

	// NATSURL is the URL coord.Open dials to reach the substrate. It
	// never appears in any coord public method signature per ADR 0003;
	// it lives on Config because it is operator-supplied input.
	NATSURL string

	// ChatFossilRepoPath is the filesystem path at which coord.Open
	// creates or opens this agent's chat Fossil repo. The operator owns
	// cleanup; coord never calls RemoveAll. In tests, pass t.TempDir().
	ChatFossilRepoPath string

	// CheckoutRoot is the absolute directory under which per-agent
	// working-copy checkouts live per ADR 0010. Coord writes to
	// CheckoutRoot/<AgentID>/. In tests, pass t.TempDir().
	CheckoutRoot string

	// ProjectPrefix scopes chat threads, presence, and KV subjects.
	// Zero means "derive from AgentID" (everything before the last
	// '-'); single-agent callers can leave it blank. Multi-process
	// flows where the agent identities don't share a prefix (e.g.
	// dispatch worker = parentID + "/" + taskID) must set this
	// explicitly to the workspace identity so all participants meet
	// on the same NATS subject namespace.
	ProjectPrefix string

	// Tuning carries substrate knobs. Zero means "use sane defaults".
	Tuning TuningConfig
}

// DeriveProjectPrefix exposes the default ProjectPrefix derivation —
// everything in agentID before the last '-' — for callers that need to
// build a Config whose AgentID does not itself contain a sensible
// project prefix (e.g. the dispatch-worker compound identity). Mirrors
// the unexported projectPrefix used by Open's default path.
func DeriveProjectPrefix(agentID string) string {
	return projectPrefix(agentID)
}

// Validate checks every Config field against its documented bounds and
// returns the first violation as an error. Validate is pure; it does
// not panic on bad operator input per invariant 9.
//
// Callers must apply defaultTuning before Validate; Open does this
// automatically. Direct Validate callers (tests) should call
// defaultTuning themselves if they rely on zero-value Tuning fields.
func (c Config) Validate() error {
	if c.AgentID == "" {
		return fmt.Errorf("coord.Config: AgentID: must be non-empty")
	}
	if c.NATSURL == "" {
		return fmt.Errorf("coord.Config: NATSURL: must be non-empty")
	}
	if c.ChatFossilRepoPath == "" {
		return fmt.Errorf(
			"coord.Config: ChatFossilRepoPath: must be non-empty",
		)
	}
	if c.CheckoutRoot == "" {
		return fmt.Errorf(
			"coord.Config: CheckoutRoot: must be non-empty",
		)
	}
	t := c.Tuning
	if t.HoldTTLDefault <= 0 {
		return fmt.Errorf("coord.Config: Tuning.HoldTTLDefault: must be > 0")
	}
	if t.HoldTTLMax <= 0 {
		return fmt.Errorf("coord.Config: Tuning.HoldTTLMax: must be > 0")
	}
	if t.HoldTTLDefault > t.HoldTTLMax {
		return fmt.Errorf(
			"coord.Config: Tuning.HoldTTLDefault: must be <= HoldTTLMax",
		)
	}
	if t.MaxHoldsPerClaim <= 0 {
		return fmt.Errorf("coord.Config: Tuning.MaxHoldsPerClaim: must be > 0")
	}
	if t.MaxSubscribers <= 0 {
		return fmt.Errorf("coord.Config: Tuning.MaxSubscribers: must be > 0")
	}
	if t.MaxTaskFiles <= 0 {
		return fmt.Errorf("coord.Config: Tuning.MaxTaskFiles: must be > 0")
	}
	if t.MaxReadyReturn <= 0 {
		return fmt.Errorf("coord.Config: Tuning.MaxReadyReturn: must be > 0")
	}
	if t.MaxTaskValueSize <= 0 {
		return fmt.Errorf("coord.Config: Tuning.MaxTaskValueSize: must be > 0")
	}
	if t.TaskHistoryDepth == 0 {
		return fmt.Errorf("coord.Config: Tuning.TaskHistoryDepth: must be > 0")
	}
	if t.HeartbeatInterval <= 0 {
		return fmt.Errorf("coord.Config: Tuning.HeartbeatInterval: must be > 0")
	}
	if t.NATSReconnectWait <= 0 {
		return fmt.Errorf("coord.Config: Tuning.NATSReconnectWait: must be > 0")
	}
	if t.NATSMaxReconnects <= 0 {
		return fmt.Errorf("coord.Config: Tuning.NATSMaxReconnects: must be > 0")
	}
	return nil
}
