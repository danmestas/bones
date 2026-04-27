package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ensureGitignoreEntries appends Fossil + orchestrator entries to the
// project's root .gitignore if they're not already present. Per ADR
// 0024: the orchestrator opens a Fossil checkout at the project root,
// so .fslckout and .fossil-settings/ must be gitignored, and
// .orchestrator/ holds runtime state that should never be committed.
//
// Idempotent: skips entries already present (whole-line match).
// Creates .gitignore if missing.
func ensureGitignoreEntries(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	wantEntries := []string{
		".fslckout",
		".fossil-settings/",
		".orchestrator/",
	}

	existing := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			existing[strings.TrimSpace(sc.Text())] = true
		}
		_ = f.Close()
	}

	var missing []string
	for _, e := range wantEntries {
		if !existing[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open .gitignore: %w", err)
	}
	defer f.Close()

	header := "\n# Orchestrator runtime + Fossil checkout-at-root (ADR 0024)\n"
	if _, err := f.WriteString(header); err != nil {
		return fmt.Errorf("write .gitignore: %w", err)
	}
	for _, e := range missing {
		if _, err := f.WriteString(e + "\n"); err != nil {
			return fmt.Errorf("write .gitignore: %w", err)
		}
	}
	return nil
}
