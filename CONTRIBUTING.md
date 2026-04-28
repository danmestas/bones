# Contributing to bones

Thanks for your interest in `bones`, a multi-agent orchestration framework
combining Fossil (durable state) with NATS (real-time coordination). This
document covers everything you need to get a working checkout, run the tests,
and submit changes.

## Development setup

Requires Go 1.26 or newer.

```
git clone https://github.com/danmestas/bones
cd bones
make
git config core.hooksPath .githooks
```

`make` builds `bin/bones`. EdgeSync is pulled in as a normal Go module
dependency for the main binary — no sibling checkout needed.

To run integration tests that exercise the leaf binary (`make leaf`), clone
EdgeSync as a sibling:

```
git clone https://github.com/danmestas/EdgeSync ../EdgeSync
make leaf
```

The pre-commit hook at `.githooks/pre-commit` runs the same gate as CI;
pointing `core.hooksPath` at it catches problems before they hit a PR.

For end-user installation (running bones, not building it), see
[GETTING_STARTED.md](./GETTING_STARTED.md).

## Running tests

```
make check       # fmt-check, vet, lint, race, todo-check
make test        # unit tests
make race        # race-detector tests
```

`make check` is the canonical pre-PR gate — the same checks run in CI.

## Code layout

- `cmd/`, `bin/` — CLI entry points (`bones`, `leaf`).
- `internal/` — implementation packages, not exported.
- `pkg/` — exported packages.
- `docs/adr/` — 21 architecture decision records documenting design rationale
  and trade-offs; read the relevant ADRs before proposing structural changes.
- `scripts/`, `.orchestrator/` — orchestrator and bootstrap scaffolding.

## Submitting changes

1. Open a feature branch off `main`. Direct commits to `main` are not accepted.
2. Run `make check` locally before pushing.
3. Open a PR; CI will re-run `make check`.
4. Significant design changes should land an ADR in `docs/adr/` either before
   or alongside the implementation PR.

## Reporting issues

Open an issue at https://github.com/danmestas/bones/issues with reproduction
steps, expected vs. actual behavior, and the output of `bones --version`.
