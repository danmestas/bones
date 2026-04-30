# ADR 0040: Default-on telemetry to Axiom for release binaries

## Context

ADR 0039 made telemetry opt-in via two environment variables and a build tag. In the four weeks since, that contract proved too restrictive in one direction and too permissive in another.

Too restrictive: nobody enabled it. The bones author has zero observability into the failure modes the substrate is meant to surface — hub-bind collisions, scaffold drift, swarm-commit failures, multi-workspace races. The OTLP wiring exists but the data never leaves an operator's machine because setting `BONES_TELEMETRY=1` plus an endpoint URL plus headers is friction operators don't pay without a personal stake.

Too permissive: the `-tags=otel` build with env vars set will export to *any* endpoint. Operators who want to help bones get fixed must manage credentials for an OTLP backend bones doesn't run, and the bones author has no shared destination to look at when a bug surfaces. Aggregate signal across installs is structurally impossible.

The product call: bones author runs Axiom, owns an ingest dataset, and wants release binaries to phone home by default with a one-command opt-out. Source-built binaries (`go install`, `make bones` without the otel tag) remain zero-egress. The single-token default ships only with goreleaser-built release artifacts.

## Decision

Release binaries default-on; source builds default-off. Opt-out is a single CLI command. No env vars required for the common path.

### Build-time gating (unchanged from ADR 0039)

`-tags=otel` still gates the OTLP exporter dependency. Source builds without the tag remain zero-egress regardless of all other configuration. The change is that goreleaser now sets the tag for every release build, and ldflag-injects the Axiom token at build time.

```yaml
# .goreleaser.yml
builds:
  - flags: [-tags=otel]
    ldflags:
      - -X github.com/danmestas/bones/internal/telemetry.axiomToken={{.Env.AXIOM_INGEST_TOKEN}}
```

The token lives in Doppler (or a GitHub Actions secret synced from Doppler). Local releases use `doppler run -- goreleaser release ...`. CI either syncs the secret via Doppler's GitHub integration or pulls it via a Doppler service token. Either way, the token never lands in source.

A release binary built with an empty `axiomToken` (e.g. forgot the env var) compiles successfully but behaves as a source build: zero egress. This is the safety net for the "release script forgot the secret" failure mode.

### Runtime resolution order

The exporter wakes up only when telemetry is *on*. Resolution, top-down:

1. `BONES_TELEMETRY=0` env var → off. Power-user kill switch for CI / sandboxed environments. Survives any other state.
2. `~/.bones/no-telemetry` file present → off. Persistent opt-out written by `bones telemetry disable`.
3. `BONES_OTEL_ENDPOINT` env var set → on, exporting to that endpoint. Self-host override; preserves ADR 0039's advanced-user path.
4. `axiomToken` ldflag-injected and non-empty → on, exporting to baked-in Axiom config. The release-binary default.
5. Otherwise → off. Source builds, dev binaries, and release binaries with empty token land here.

### Baked-in Axiom config

```go
var (
    axiomToken    = ""                                    // injected at release build
    axiomDataset  = "bones-prod"                          // baked
    axiomEndpoint = "https://api.axiom.co/v1/traces"      // baked
)
```

OTLP HTTP request shape: `Authorization: Bearer <axiomToken>`, `X-Axiom-Dataset: bones-prod`. The dataset and endpoint are hardcoded so a stolen token can only spray one dataset; rotating the token (Axiom UI → regenerate) cuts off abuse without a code change.

### Opt-out UX

Three commands:

```
bones telemetry              # show current state + endpoint + install_id
bones telemetry disable      # write ~/.bones/no-telemetry
bones telemetry enable       # remove ~/.bones/no-telemetry
```

`bones telemetry status` is the alias for the bare `bones telemetry`. The bare verb is the one we mention everywhere — fewer subcommands to remember.

The opt-out is a file, not an env var, because env vars are ephemeral and shell-specific. A user who runs `bones telemetry disable` once expects the setting to survive a new terminal, a reboot, and an upgrade. The file is the durable surface.

### First-run notice

On the first run with telemetry enabled (no `~/.bones/telemetry-acknowledged` marker), bones prints to stderr exactly one line and creates the marker:

```
bones: anonymous usage telemetry enabled — bones-prod dataset on Axiom. Opt out: bones telemetry disable
```

The notice is loud and unsilenceable per ADR 0039. Subsequent runs are silent until the marker disappears. Removing the marker re-prompts.

### Span attribute policy (unchanged from ADR 0039)

Span attributes remain bool / int64 / 12-char `workspace_hash`. No paths, no error strings, no slot/task names. PII risk is structural: the policy is enforced by the verb instrumentation, not by a runtime scrubber. ADR 0039 §"Span attribute policy" is the load-bearing description and is unchanged.

`internal/telemetry/scrub.go` (workspace-path → hash) continues to gate every span attribute that could leak a path. New attributes that aren't bool/int/hash require a follow-up ADR per ADR 0039.

### `bones doctor` surface (extended)

```
=== telemetry (ADR 0040) ===
  on   endpoint=https://api.axiom.co/v1/traces dataset=bones-prod install_id=<uuid>

=== telemetry (ADR 0040) ===
  off — disabled by ~/.bones/no-telemetry

=== telemetry (ADR 0040) ===
  off — built without -tags=otel (source build, no exporter)

=== telemetry (ADR 0040) ===
  off — release build but axiomToken empty (release misconfigured)
```

The `release misconfigured` line surfaces the "build forgot the secret" case. Operators see it; the bones author sees it via support reports and knows the next release script ran without the env var.

## Consequences

- **Aggregate signal is unblocked.** Every release-binary install phones home by default; the dataset accumulates real-world failure-mode counts the bones author can act on.
- **Trust burden is high.** Default-on telemetry without consent burns trust the moment a privacy-conscious user discovers it post-install. The first-run notice + one-command opt-out are the entire defense; both have to be perfect. The notice text, the disable command, and the Axiom dataset name are now contract surfaces — changing them requires a follow-up ADR.
- **Token is extractable from the binary.** `strings bones | grep -i token` recovers the Axiom ingest token from any release binary. Mitigations: ingest-only API token (write-only, scoped to one dataset) → a stolen token cannot read or delete data; Axiom usage alerts on the dataset → cost spikes are visible immediately; rotating the token in Axiom UI cuts off abuse without a code change. The cost ceiling per month is bounded by Axiom's plan limits — the bones author accepts this as the price of default-on.
- **Source builds remain trust-preserved.** `go install` produces a no-egress binary. A privacy-conservative user who builds bones from source has the same guarantee they had under ADR 0039.
- **Reverses ADR 0039's "default off" promise.** Anyone reading 0039 in isolation will form the wrong mental model. 0039 must be marked superseded; the codebase must point both surfaces (notice text, doctor output) at this ADR's number.
- **Opt-out file is the new contract.** Path `~/.bones/no-telemetry`. Don't move it without a migration; users who set it expect it to keep working across upgrades.

## Alternatives considered

**Keep ADR 0039 — opt-in only.** Rejected. After four weeks of zero data, the cost of the privacy-maximal stance is no observability at all. The release-only default-on with one-command opt-out is the better tradeoff for a tool whose maintainer needs the data to make the tool better.

**Default-on for both source and release builds.** Rejected. Source builds are dev-loop binaries; the developer running `go install` from source is shipping a binary that may be embedded in unrelated tooling, CI, or test environments where phone-home is surprising. The build-tag gate is cheap to keep and the safety it provides is real.

**Persisted opt-in (config file, not default-on).** Considered. Friendlier than env vars per ADR 0039 §"Persisted opt-in." Rejected because it's the same friction shape — most users will never run the enable command, so the dataset stays empty. The product call is "default-on with one-command opt-out" not "default-off with one-command opt-in."

**Anonymized vendor (Sentry, PostHog, Honeycomb).** Defer. Axiom is the maintainer's existing tool; switching vendor is a separate decision once the data shape stabilizes.

**Sample at the source.** Defer. Cost-control is currently handled by Axiom's plan and ingest-token rate limits. If volume becomes a problem, the next ADR adds a `sample_rate` ldflag-injected variable.

## Status

Accepted, 2026-04-30. Supersedes ADR 0039.
