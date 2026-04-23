# Default compaction provider + cadence wrapper Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bind `coord.Compact` to a default Anthropic-backed summarizer outside `coord` and expose it through `agent-tasks compact`, including an explicit repeat cadence wrapper.

**Architecture:** Keep `coord` provider-agnostic. Add a small Anthropic client package that implements `coord.Summarizer`, then add a CLI subcommand that builds the summarizer from environment variables and either runs once or on a fixed `--every` interval.

**Tech Stack:** Go stdlib `net/http`, existing `coord` and `cmd/agent-tasks` packages, existing test harnesses.

---
