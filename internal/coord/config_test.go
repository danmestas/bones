package coord

import (
	"strings"
	"testing"
	"time"
)

// baselineConfig returns a fully-valid Config used as the starting
// point for every Validate subtest. Tuning fields are set explicitly
// so mutation tests can zero individual fields below the default.
func baselineConfig() Config {
	return Config{
		AgentID:      "test-agent",
		NATSURL:      "nats://127.0.0.1:4222",
		CheckoutRoot: "/tmp/coord-baseline-checkouts",
		Tuning: TuningConfig{
			HoldTTLDefault:    30 * time.Second,
			HoldTTLMax:        5 * time.Minute,
			MaxHoldsPerClaim:  32,
			MaxSubscribers:    32,
			MaxTaskFiles:      32,
			MaxReadyReturn:    256,
			MaxTaskValueSize:  8 * 1024,
			TaskHistoryDepth:  8,
			HeartbeatInterval: 5 * time.Second,
			NATSReconnectWait: 2 * time.Second,
			NATSMaxReconnects: 5,
		},
	}
}

func TestConfigValidate_Valid(t *testing.T) {
	cfg := baselineConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("baseline config should validate, got err: %v", err)
	}
}

func TestConfigValidate_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantKey string
	}{
		{
			name:    "empty AgentID",
			mutate:  func(c *Config) { c.AgentID = "" },
			wantKey: "AgentID",
		},
		{
			name:    "zero HoldTTLDefault",
			mutate:  func(c *Config) { c.Tuning.HoldTTLDefault = 0 },
			wantKey: "HoldTTLDefault",
		},
		{
			name:    "negative HoldTTLDefault",
			mutate:  func(c *Config) { c.Tuning.HoldTTLDefault = -1 },
			wantKey: "HoldTTLDefault",
		},
		{
			name:    "zero HoldTTLMax",
			mutate:  func(c *Config) { c.Tuning.HoldTTLMax = 0 },
			wantKey: "HoldTTLMax",
		},
		{
			name: "HoldTTLDefault exceeds HoldTTLMax",
			mutate: func(c *Config) {
				c.Tuning.HoldTTLDefault = 10 * time.Minute
				c.Tuning.HoldTTLMax = 1 * time.Minute
			},
			wantKey: "HoldTTLDefault",
		},
		{
			name:    "zero MaxHoldsPerClaim",
			mutate:  func(c *Config) { c.Tuning.MaxHoldsPerClaim = 0 },
			wantKey: "MaxHoldsPerClaim",
		},
		{
			name:    "negative MaxHoldsPerClaim",
			mutate:  func(c *Config) { c.Tuning.MaxHoldsPerClaim = -1 },
			wantKey: "MaxHoldsPerClaim",
		},
		{
			name:    "zero MaxSubscribers",
			mutate:  func(c *Config) { c.Tuning.MaxSubscribers = 0 },
			wantKey: "MaxSubscribers",
		},
		{
			name:    "zero MaxTaskFiles",
			mutate:  func(c *Config) { c.Tuning.MaxTaskFiles = 0 },
			wantKey: "MaxTaskFiles",
		},
		{
			name:    "zero MaxReadyReturn",
			mutate:  func(c *Config) { c.Tuning.MaxReadyReturn = 0 },
			wantKey: "MaxReadyReturn",
		},
		{
			name:    "negative MaxReadyReturn",
			mutate:  func(c *Config) { c.Tuning.MaxReadyReturn = -1 },
			wantKey: "MaxReadyReturn",
		},
		{
			name:    "zero MaxTaskValueSize",
			mutate:  func(c *Config) { c.Tuning.MaxTaskValueSize = 0 },
			wantKey: "MaxTaskValueSize",
		},
		{
			name:    "negative MaxTaskValueSize",
			mutate:  func(c *Config) { c.Tuning.MaxTaskValueSize = -1 },
			wantKey: "MaxTaskValueSize",
		},
		{
			name:    "zero TaskHistoryDepth",
			mutate:  func(c *Config) { c.Tuning.TaskHistoryDepth = 0 },
			wantKey: "TaskHistoryDepth",
		},
		{
			name:    "zero HeartbeatInterval",
			mutate:  func(c *Config) { c.Tuning.HeartbeatInterval = 0 },
			wantKey: "HeartbeatInterval",
		},
		{
			name:    "zero NATSReconnectWait",
			mutate:  func(c *Config) { c.Tuning.NATSReconnectWait = 0 },
			wantKey: "NATSReconnectWait",
		},
		{
			name:    "zero NATSMaxReconnects",
			mutate:  func(c *Config) { c.Tuning.NATSMaxReconnects = 0 },
			wantKey: "NATSMaxReconnects",
		},
		{
			name:    "empty NATSURL",
			mutate:  func(c *Config) { c.NATSURL = "" },
			wantKey: "NATSURL",
		},
		{
			name:    "empty CheckoutRoot",
			mutate:  func(c *Config) { c.CheckoutRoot = "" },
			wantKey: "CheckoutRoot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baselineConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Fatalf(
					"error %q does not contain field name %q",
					err.Error(), tc.wantKey,
				)
			}
			if !strings.Contains(err.Error(), "coord.Config:") {
				t.Fatalf(
					"error %q missing \"coord.Config:\" prefix",
					err.Error(),
				)
			}
		})
	}
}
