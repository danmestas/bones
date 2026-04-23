# agent-infra — developer Makefile.
#
# Source of truth for local and CI discipline checks. The CI workflow invokes
# `make check` verbatim so the local and remote gates cannot drift. See
# .github/workflows/ci.yml and docs/invariants.md.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

# go list of packages, excluding the read-only reference clones. Empty on a
# bare tree (Phase 1 currently has no .go files); callers must tolerate that.
GO_PACKAGES := $(shell go list ./... 2>/dev/null)

.PHONY: check fmt fmt-check vet lint test race todo-check install-tools help agent-init agent-tasks bin

help:
	@echo "Targets:"
	@echo "  check          — full discipline suite (fmt-check, vet, lint, race, todo-check)"
	@echo "  fmt            — gofmt -w . (writes changes)"
	@echo "  fmt-check      — gofmt -l . (read-only; exits 1 on drift)"
	@echo "  vet            — go vet ./..."
	@echo "  lint           — golangci-lint run"
	@echo "  test           — go test ./..."
	@echo "  race           — go test -race ./..."
	@echo "  todo-check     — forbid TODO in non-test .go files"
	@echo "  install-tools  — install staticcheck + errcheck for local dev"

check: fmt-check vet lint race todo-check
	@echo "check: OK"

fmt:
	gofmt -w .

# Read-only formatting verifier. gofmt -l prints any file that would change;
# non-empty output is failure. We list explicit paths (excluding reference/)
# so a clean tree produces no output and exits 0.
fmt-check:
	@drift=$$(gofmt -l $$(find . -type f -name '*.go' -not -path './reference/*' -not -path './.git/*' 2>/dev/null) 2>/dev/null); \
	if [ -n "$$drift" ]; then \
		echo "gofmt drift in:" >&2; \
		echo "$$drift" >&2; \
		exit 1; \
	fi

vet:
	@if [ -n "$(GO_PACKAGES)" ]; then \
		go vet ./...; \
	else \
		echo "vet: no Go packages yet — skipping"; \
	fi

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "lint: golangci-lint not installed — skipping (CI will enforce)"; \
	fi

test:
	@if [ -n "$(GO_PACKAGES)" ]; then \
		go test ./...; \
	else \
		echo "test: no Go packages yet — skipping"; \
	fi

race:
	@if [ -n "$(GO_PACKAGES)" ]; then \
		go test -race ./...; \
	else \
		echo "race: no Go packages yet — skipping"; \
	fi

# Forbid TODO markers in production .go files. Tests may document pending
# behavior inline, so _test.go is exempt. Matches are printed with file:line.
todo-check:
	@matches=$$(find . -type f -name '*.go' \
		-not -name '*_test.go' \
		-not -path './reference/*' \
		-not -path './.git/*' \
		-exec grep -HnE 'TODO' {} + 2>/dev/null || true); \
	if [ -n "$$matches" ]; then \
		echo "TODO found in non-test .go files — file an ADR or roadmap note instead:" >&2; \
		echo "$$matches" >&2; \
		exit 1; \
	fi

# Convenience target for local dev. golangci-lint bundles staticcheck and
# errcheck, so these are redundant in CI — but useful for editor integration.
install-tools:
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install github.com/kisielk/errcheck@latest

bin:
	mkdir -p bin

agent-init: bin
	go build -o bin/agent-init ./cmd/agent-init

agent-tasks: bin
	go build -o bin/agent-tasks ./cmd/agent-tasks
