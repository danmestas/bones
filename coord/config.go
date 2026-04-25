package coord

import (
	"fmt"
	"net/url"
	"strings"
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

	// MaxReadyReturn caps the number of tasks coord.Ready returns in a
	// single call. Ready scans the tasks bucket client-side, so an
	// unbounded return would let a large bucket stall the caller and the
	// substrate proportionally. The operator supplies the number; there
	// is no silent default, and Validate rejects zero/negative.
	MaxReadyReturn int

	// MaxTaskValueSize is the upper bound on a task record's serialized
	// JSON value, in bytes, enforced at every write in internal/tasks/
	// per invariant 14. ADR 0005 recommends 8 KB; Config is the
	// enforcement point and takes no silent default — the operator
	// supplies the number, and Validate rejects zero/negative.
	MaxTaskValueSize int

	// TaskHistoryDepth is the per-key JetStream KV history depth for the
	// tasks bucket. ADR 0005 sets the recommended value at 8 (one entry
	// per write: open, claim, up to ~4 updates, close, plus slack). The
	// operator supplies the number; Validate rejects zero so there is no
	// silent default at the coord layer.
	TaskHistoryDepth uint8

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

	// ChatFossilRepoPath is the filesystem path at which coord.Open
	// creates or opens this agent's chat Fossil repo. The operator owns
	// cleanup; coord never calls RemoveAll. Pinning the location to
	// Config (as opposed to the per-Open MkdirTemp used in Phase 3A)
	// makes chat history replayable across restarts and keeps /tmp
	// from growing over a long-running agent's lifetime. In tests,
	// pass t.TempDir() — a fresh directory per test keeps two
	// concurrent Coords on one substrate from colliding on the repo.
	ChatFossilRepoPath string

	// FossilRepoPath is the absolute filesystem path to the shared Fossil
	// repo DB used for the code-artifact substrate per ADR 0010. The
	// operator owns cleanup; coord never calls RemoveAll. Distinct from
	// ChatFossilRepoPath: chat messages and code commits live in separate
	// Fossil repos so their replay streams stay untangled.
	FossilRepoPath string

	// CheckoutRoot is the absolute directory under which per-agent
	// working-copy checkouts live per ADR 0010. Coord writes to
	// CheckoutRoot/<AgentID>/. In tests, pass t.TempDir().
	CheckoutRoot string

	// HubURL is the http base URL of the orchestrator's fossil server.
	// When non-empty, coord enables hub-pull on tip.changed broadcasts
	// and pull+update+retry on commit fork detection. When empty, coord
	// behaves as in v0.x — local-only, no hub interaction.
	HubURL string

	// EnableTipBroadcast, when true and HubURL is non-empty, makes
	// coord.Commit publish a tip.changed message on NATS after every
	// successful commit, and makes coord.Open subscribe to it. Default
	// (false) preserves the v0.x no-broadcast behavior.
	EnableTipBroadcast bool
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
	if c.MaxReadyReturn <= 0 {
		return fmt.Errorf("coord.Config: MaxReadyReturn: must be > 0")
	}
	if c.MaxTaskValueSize <= 0 {
		return fmt.Errorf("coord.Config: MaxTaskValueSize: must be > 0")
	}
	if c.TaskHistoryDepth == 0 {
		return fmt.Errorf("coord.Config: TaskHistoryDepth: must be > 0")
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
	if c.ChatFossilRepoPath == "" {
		return fmt.Errorf(
			"coord.Config: ChatFossilRepoPath: must be non-empty",
		)
	}
	if c.FossilRepoPath == "" {
		return fmt.Errorf(
			"coord.Config: FossilRepoPath: must be non-empty",
		)
	}
	if c.CheckoutRoot == "" {
		return fmt.Errorf(
			"coord.Config: CheckoutRoot: must be non-empty",
		)
	}
	if err := validateHubURL(c.HubURL); err != nil {
		return err
	}
	return nil
}

// validateHubURL enforces the HubURL contract: empty is fine (local-only
// mode); otherwise the value must parse as a valid URI and use http(s).
func validateHubURL(hubURL string) error {
	if hubURL == "" {
		return nil
	}
	if _, err := url.ParseRequestURI(hubURL); err != nil {
		return fmt.Errorf("coord.Config: HubURL: %w", err)
	}
	if !strings.HasPrefix(hubURL, "http://") &&
		!strings.HasPrefix(hubURL, "https://") {
		return fmt.Errorf(
			"coord.Config: HubURL: must start with http:// or https://",
		)
	}
	return nil
}
