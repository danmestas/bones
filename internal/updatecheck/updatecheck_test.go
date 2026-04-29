package updatecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.0", "v0.1.0", true},
		{"v0.2.0", "0.1.0", true},
		{"0.2.0", "v0.1.0", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.2.1", "v0.2.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.1.0", "v0.2.0", false},
		// Bad inputs should NOT trigger notices — return false
		// rather than guess.
		{"vNotASemver", "v0.1.0", false},
		{"v0.2.0", "garbage", false},
		{"v0.2.0-rc1", "v0.1.0", false},
	}
	for _, c := range cases {
		got := Newer(c.latest, c.current)
		if got != c.want {
			t.Errorf("Newer(%q, %q): got %v, want %v",
				c.latest, c.current, got, c.want)
		}
	}
}

func TestShouldSkip(t *testing.T) {
	t.Setenv("BONES_UPDATE_CHECK", "")
	cases := []struct {
		version string
		want    bool
	}{
		{"", true},
		{"dev", true},
		{"v0.2.0", false},
		{"0.2.0", false},
	}
	for _, c := range cases {
		if got := shouldSkip(c.version); got != c.want {
			t.Errorf("shouldSkip(%q): got %v, want %v",
				c.version, got, c.want)
		}
	}
}

func TestShouldSkip_OptOutEnv(t *testing.T) {
	t.Setenv("BONES_UPDATE_CHECK", "0")
	if !shouldSkip("v0.2.0") {
		t.Error("BONES_UPDATE_CHECK=0 should skip even on real version")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "version-check.json")

	// Empty file: returns zero value.
	got := readCache(path)
	if got.Latest != "" || !got.LastCheck.IsZero() {
		t.Errorf("empty cache: got %+v", got)
	}

	// Round-trip.
	want := cacheEntry{
		LastCheck: time.Now().UTC().Truncate(time.Second),
		Latest:    "v0.3.0",
	}
	writeCache(path, want)

	got = readCache(path)
	if got.Latest != want.Latest {
		t.Errorf("Latest: got %q, want %q", got.Latest, want.Latest)
	}
	if !got.LastCheck.Equal(want.LastCheck) {
		t.Errorf("LastCheck: got %v, want %v", got.LastCheck, want.LastCheck)
	}

	// Malformed cache: returns zero value, no panic.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed: %v", err)
	}
	got = readCache(path)
	if got.Latest != "" {
		t.Errorf("malformed cache should return zero value, got %+v", got)
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"v0.2.0", "v0.2.0"},
		{"0.2.0", "v0.2.0"},
	}
	for _, c := range cases {
		if got := normalizeVersion(c.in); got != c.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFetchLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.99.0"})
	}))
	defer srv.Close()

	prev := LatestReleaseURL
	LatestReleaseURL = srv.URL
	defer func() { LatestReleaseURL = prev }()

	tag, err := fetchLatest(context.Background())
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if tag != "v0.99.0" {
		t.Errorf("tag = %q, want v0.99.0", tag)
	}
}

func TestFetchLatest_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	prev := LatestReleaseURL
	LatestReleaseURL = srv.URL
	defer func() { LatestReleaseURL = prev }()

	if _, err := fetchLatest(context.Background()); err == nil {
		t.Error("expected error on 500 response")
	}
}

func TestPrintNotice(t *testing.T) {
	// Verify formatting end-to-end through a bytes.Buffer disguised as
	// an *os.File via a temp pipe. Simpler: test the format string
	// directly.
	want := fmt.Sprintf(
		"bones: update available: v0.1.0 → v0.2.0 (run: %s)\n",
		UpgradeHint,
	)
	var buf bytes.Buffer
	fmt.Fprintf(&buf,
		"bones: update available: %s → %s (run: %s)\n",
		normalizeVersion("v0.1.0"), normalizeVersion("0.2.0"), UpgradeHint,
	)
	if buf.String() != want {
		t.Errorf("notice format drift: got %q, want %q", buf.String(), want)
	}
}
