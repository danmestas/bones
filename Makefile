# bones — developer Makefile.
#
# Source of truth for local and CI discipline checks. The CI workflow invokes
# `make check` verbatim so the local and remote gates cannot drift. See
# .github/workflows/ci.yml and docs/invariants.md.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

# go list of packages, excluding the read-only reference clones. Empty on a
# bare tree (Phase 1 currently has no .go files); callers must tolerate that.
GO_PACKAGES := $(shell go list ./... 2>/dev/null)

.PHONY: check fmt fmt-check vet lint test race todo-check install-tools help bones bin

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
		go test -race -short ./...; \
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

bones: bin
	go build -o bin/bones ./cmd/bones

# CI mirror — runs make check + otel lanes from .github/workflows/ci.yml.
# Cross-repo leaf-binary integration is CI-only.
.PHONY: ci ci-fast
ci:
	$(MAKE) check
	go build -tags=otel ./...
	go test -tags=otel -short ./... -count=1

# Fast subset for pre-push hook (~30-60s; no -tags=otel, no -race).
ci-fast:
	go vet ./...
	go build ./...
	go test -short -count=1 -timeout=30s ./...

.PHONY: release
release:
	@test -n "$(VERSION)" || { echo "VERSION=vX.Y.Z required"; exit 1; }
	@echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$$' || { echo "bad version format: $(VERSION)"; exit 1; }
	@git diff --quiet || { echo "tree dirty; commit or stash first"; exit 1; }
	@git fetch origin --tags
	@if git rev-parse "$(VERSION)" >/dev/null 2>&1; then echo "tag $(VERSION) already exists"; exit 1; fi
	@$(MAKE) ci
	@PREV=$$(git describe --tags --abbrev=0 2>/dev/null || echo ""); \
	  TMPL=.github/RELEASE_TEMPLATE.md; \
	  TMP=$$(mktemp); \
	  { echo "Release $(VERSION)"; echo; \
	    [ -f $$TMPL ] && { cat $$TMPL; echo; }; \
	    echo "## Changes"; \
	    if [ -n "$$PREV" ]; then git log --oneline $$PREV..HEAD; else git log --oneline; fi; } > $$TMP; \
	  $${EDITOR:-vi} $$TMP; \
	  git tag -a "$(VERSION)" -F $$TMP; \
	  rm $$TMP
	@echo ""
	@echo "Tag $(VERSION) created locally. To publish:"
	@echo "  git push origin $(VERSION)"

.PHONY: setup-hooks
setup-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks configured to use .githooks/ directory."
	@echo "Pre-push runs make ci-fast (~60s). Skip with: git push --no-verify"
