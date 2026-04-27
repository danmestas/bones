package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureGitignoreEntries_FreshFile(t *testing.T) {
	dir := t.TempDir()
	if err := ensureGitignoreEntries(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".fslckout", ".fossil-settings/", ".orchestrator/"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q\n%s", want, data)
		}
	}
}

func TestEnsureGitignoreEntries_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := ensureGitignoreEntries(dir); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureGitignoreEntries(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf(".gitignore changed on second run\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestEnsureGitignoreEntries_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	preexisting := "node_modules/\n*.log\n"
	if err := os.WriteFile(path, []byte(preexisting), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureGitignoreEntries(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "node_modules/") {
		t.Errorf("preexisting entry lost:\n%s", data)
	}
	if !strings.Contains(string(data), ".fslckout") {
		t.Errorf("new entry missing:\n%s", data)
	}
}

func TestEnsureGitignoreEntries_PartialOverlap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	// File already has .fslckout but not the other two.
	preexisting := ".fslckout\nnode_modules/\n"
	if err := os.WriteFile(path, []byte(preexisting), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureGitignoreEntries(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	// .fslckout must appear exactly once (no duplicate added).
	if strings.Count(body, ".fslckout\n") != 1 {
		t.Errorf(".fslckout duplicated\n%s", body)
	}
	// The other two entries must now be present.
	for _, want := range []string{".fossil-settings/", ".orchestrator/"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n%s", want, body)
		}
	}
	// Preexisting unrelated entry must survive.
	if !strings.Contains(body, "node_modules/") {
		t.Errorf("preexisting entry lost\n%s", body)
	}
}
