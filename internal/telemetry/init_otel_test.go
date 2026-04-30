//go:build otel

package telemetry

import (
	"strings"
	"testing"
)

// resetAxiomVars rewires the build-time-injected vars for a single
// test, restoring them on cleanup. Lets tests exercise the
// "release build with token" and "source build with empty token"
// branches of resolve() without rebuilding.
func resetAxiomVars(t *testing.T, token, dataset, endpoint string) {
	t.Helper()
	prevToken, prevDataset, prevEndpoint := axiomToken, axiomDataset, axiomEndpoint
	t.Cleanup(func() {
		axiomToken = prevToken
		axiomDataset = prevDataset
		axiomEndpoint = prevEndpoint
	})
	axiomToken = token
	axiomDataset = dataset
	axiomEndpoint = endpoint
}

// TestResolve_KillSwitchWins pins the highest-priority signal:
// BONES_TELEMETRY=0 must disable telemetry regardless of the
// opt-out file, env endpoint, or baked Axiom token. The kill
// switch is the CI/sandbox escape hatch and must be load-bearing.
func TestResolve_KillSwitchWins(t *testing.T) {
	withTempHome(t)
	t.Setenv("BONES_TELEMETRY", "0")
	t.Setenv("BONES_OTEL_ENDPOINT", "https://override")
	resetAxiomVars(t, "fake-token", "bones-prod", "https://api.axiom.co/v1/traces")

	cfg := resolve()
	if cfg.enabled {
		t.Errorf("resolve: want off, got enabled=%v reason=%q", cfg.enabled, cfg.reason)
	}
	if !strings.Contains(cfg.reason, "BONES_TELEMETRY=0") {
		t.Errorf("reason missing kill switch citation: %q", cfg.reason)
	}
}

// TestResolve_OptOutFileBeatsBakedToken verifies that the
// persistent opt-out file takes precedence over a release build's
// baked Axiom token. The whole point of the opt-out file is that
// it survives upgrades; if a baked token could override it,
// upgrading to a new release would re-enable telemetry on a user
// who'd disabled it. Pin that.
func TestResolve_OptOutFileBeatsBakedToken(t *testing.T) {
	withTempHome(t)
	if err := Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	resetAxiomVars(t, "fake-token", "bones-prod", "https://api.axiom.co/v1/traces")

	cfg := resolve()
	if cfg.enabled {
		t.Errorf("resolve: want off, got enabled=%v reason=%q", cfg.enabled, cfg.reason)
	}
	if !strings.Contains(cfg.reason, "no-telemetry") {
		t.Errorf("reason missing opt-out citation: %q", cfg.reason)
	}
}

// TestResolve_EnvOverrideUsesUserEndpoint covers the self-host
// path: BONES_OTEL_ENDPOINT set means a user with their own
// SigNoz/Tempo gets their endpoint, not the baked Axiom one.
// The override is the load-bearing affordance for ADR 0039
// users who don't want their data in the bones-prod dataset.
func TestResolve_EnvOverrideUsesUserEndpoint(t *testing.T) {
	withTempHome(t)
	t.Setenv("BONES_OTEL_ENDPOINT", "https://my-signoz/v1/traces")
	resetAxiomVars(t, "fake-token", "bones-prod", "https://api.axiom.co/v1/traces")

	cfg := resolve()
	if !cfg.enabled {
		t.Fatalf("resolve: want enabled, got off reason=%q", cfg.reason)
	}
	if cfg.endpoint != "https://my-signoz/v1/traces" {
		t.Errorf("endpoint: got %q, want override", cfg.endpoint)
	}
	if _, hasAuth := cfg.headers["Authorization"]; hasAuth {
		t.Errorf("override path leaked Axiom Authorization header: %v", cfg.headers)
	}
}

// TestResolve_BakedTokenIsDefaultOn pins the ADR 0040 default:
// release binary, no opt-out, no env vars → exports to baked
// Axiom config with the dataset header set. This is the
// "1000% DX" path the ADR is built around.
func TestResolve_BakedTokenIsDefaultOn(t *testing.T) {
	withTempHome(t)
	resetAxiomVars(t, "fake-token", "bones-prod", "https://api.axiom.co/v1/traces")

	cfg := resolve()
	if !cfg.enabled {
		t.Fatalf("resolve: want enabled, got off reason=%q", cfg.reason)
	}
	if cfg.endpoint != "https://api.axiom.co/v1/traces" {
		t.Errorf("endpoint: got %q, want baked", cfg.endpoint)
	}
	if cfg.headers["Authorization"] != "Bearer fake-token" {
		t.Errorf("Authorization header: got %q", cfg.headers["Authorization"])
	}
	if cfg.headers[axiomDatasetHeader] != "bones-prod" {
		t.Errorf("dataset header: got %q, want bones-prod", cfg.headers[axiomDatasetHeader])
	}
}

// TestResolve_EmptyTokenIsOff covers the safety-net case for
// release builds where the goreleaser env var was missing — the
// binary compiles successfully but has no Axiom credentials.
// Resolution must land at off with a reason that surfaces the
// misconfiguration to anyone running `bones doctor`.
func TestResolve_EmptyTokenIsOff(t *testing.T) {
	withTempHome(t)
	resetAxiomVars(t, "", "bones-prod", "https://api.axiom.co/v1/traces")

	cfg := resolve()
	if cfg.enabled {
		t.Errorf("resolve: want off when token empty, got enabled=%v", cfg.enabled)
	}
	if !strings.Contains(cfg.reason, "axiomToken empty") {
		t.Errorf("reason should surface release misconfiguration: %q", cfg.reason)
	}
}

// TestDataset_EmptyOnEnvOverride verifies that Dataset() returns
// "" when the user is exporting via BONES_OTEL_ENDPOINT, even if
// the binary has a baked Axiom token. The dataset is bones-prod
// only — surfacing it on a self-host path would lie to the user
// about where their data goes.
func TestDataset_EmptyOnEnvOverride(t *testing.T) {
	withTempHome(t)
	t.Setenv("BONES_OTEL_ENDPOINT", "https://my-signoz/v1/traces")
	resetAxiomVars(t, "fake-token", "bones-prod", "https://api.axiom.co/v1/traces")

	if got := Dataset(); got != "" {
		t.Errorf("Dataset on env override: got %q, want \"\"", got)
	}
}
