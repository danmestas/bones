package scaffoldver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadMissing(t *testing.T) {
	root := t.TempDir()
	got, err := Read(root)
	if err != nil {
		t.Fatalf("Read on missing stamp: unexpected error %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestWriteThenRead(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, "0.3.0"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != "0.3.0" {
		t.Errorf("Read: got %q, want %q", got, "0.3.0")
	}
}

func TestWriteCreatesParentDir(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, "0.3.1"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".bones", "scaffold_version")); err != nil {
		t.Errorf("stamp not at expected path: %v", err)
	}
}

func TestWriteIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, "0.3.0"); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(root, ".bones", "scaffold_version"))
	if err := Write(root, "0.3.0"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(root, ".bones", "scaffold_version"))
	if string(first) != string(second) {
		t.Errorf("repeated write differed: %q vs %q", first, second)
	}
}

func TestDriftedCases(t *testing.T) {
	cases := []struct {
		name   string
		stamp  string
		binary string
		want   bool
	}{
		{"fresh workspace no stamp", "", "0.3.1", false},
		{"dev binary suppresses warning", "0.3.0", "dev", false},
		{"empty binary suppresses warning", "0.3.0", "", false},
		{"identical version", "0.3.0", "0.3.0", false},
		{"different version", "0.3.0", "0.3.1", true},
		{"both empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Drifted(tc.stamp, tc.binary); got != tc.want {
				t.Errorf("Drifted(%q, %q) = %v, want %v",
					tc.stamp, tc.binary, got, tc.want)
			}
		})
	}
}
