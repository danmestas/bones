// Package updatecheck queries GitHub for the latest bones release tag
// and emits a one-line stderr notice when the running binary is
// behind. Best-effort and rate-limited (once per 24h cached on
// disk). Called from cmd/bones/main.go at process start.
//
// Design notes:
//
//   - The notice is printed synchronously from the on-disk cache. The
//     network refresh runs asynchronously and may not finish before
//     a short-lived CLI process exits — that's OK; the next long-
//     enough invocation picks up the new latest.
//   - "dev" builds (no version embedded) skip the check entirely so
//     local hacking never triggers a network call.
//   - Suppressed by env var BONES_UPDATE_CHECK=0.
//   - All errors are silent: an update check that fails must never
//     interfere with the verb the user actually ran.
package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LatestReleaseURL is the GitHub API endpoint queried for the latest
// release tag. Exported so tests can override it without monkey-patching.
var LatestReleaseURL = "https://api.github.com/repos/danmestas/bones/releases/latest"

// CacheTTL is the minimum interval between network refreshes.
// Exported for tests; production callers leave it at 24h.
var CacheTTL = 24 * time.Hour

// FetchTimeout caps how long the async refresh waits for GitHub.
var FetchTimeout = 3 * time.Second

// UpgradeHint is the user-facing instruction appended to every notice.
// One-line so the notice fits on a single stderr row.
var UpgradeHint = "brew upgrade danmestas/tap/bones"

type cacheEntry struct {
	LastCheck time.Time `json:"last_check"`
	Latest    string    `json:"latest"`
}

// Check is the public entry point. Reads the cache, prints a notice
// to stderr if the running binary is behind, and spawns an async
// refresh when the cache is stale. Never blocks the caller for the
// network round-trip.
func Check(currentVersion string) {
	if shouldSkip(currentVersion) {
		return
	}
	cachePath, err := cacheFile()
	if err != nil {
		return
	}

	entry := readCache(cachePath)
	if entry.Latest != "" && Newer(entry.Latest, currentVersion) {
		printNotice(os.Stderr, currentVersion, entry.Latest)
	}

	if time.Since(entry.LastCheck) < CacheTTL {
		return
	}

	go refresh(cachePath)
}

// shouldSkip returns true when the update check must not run: dev
// builds, opt-out env var, or empty version.
func shouldSkip(currentVersion string) bool {
	if currentVersion == "" || currentVersion == "dev" {
		return true
	}
	if os.Getenv("BONES_UPDATE_CHECK") == "0" {
		return true
	}
	return false
}

// cacheFile resolves the on-disk path used to memoize the latest
// observed release tag. Uses os.UserCacheDir per platform conventions.
func cacheFile() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	bonesDir := filepath.Join(dir, "bones")
	if err := os.MkdirAll(bonesDir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(bonesDir, "version-check.json"), nil
}

// readCache loads the cache entry from path. A missing or malformed
// cache returns a zero-value entry; callers treat it as "never
// checked, no known latest."
func readCache(path string) cacheEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return cacheEntry{}
	}
	var e cacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return cacheEntry{}
	}
	return e
}

// writeCache persists the entry. Errors are intentionally ignored —
// a failed write means the next invocation will refresh again, which
// is the correct behavior.
func writeCache(path string, entry cacheEntry) {
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// refresh fetches the latest release tag from GitHub and writes the
// cache. Runs in its own goroutine; never returns errors visibly.
// On fetch failure the cache is unchanged so the next invocation
// will retry.
func refresh(cachePath string) {
	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
	defer cancel()
	latest, err := fetchLatest(ctx)
	if err != nil {
		return
	}
	writeCache(cachePath, cacheEntry{LastCheck: time.Now(), Latest: latest})
}

// fetchLatest queries GitHub's "latest release" endpoint and returns
// the tag_name string ("v0.2.0"). Exposed for tests.
func fetchLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LatestReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: status %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", fmt.Errorf("github: empty tag_name")
	}
	return body.TagName, nil
}

// printNotice formats and emits the one-line stderr update notice.
// Both versions are normalized to a leading "v" so the message reads
// "v0.2.0 → v0.3.0" regardless of which form GoReleaser injected.
func printNotice(w *os.File, current, latest string) {
	_, _ = fmt.Fprintf(w,
		"bones: update available: %s → %s (run: %s)\n",
		normalizeVersion(current), normalizeVersion(latest), UpgradeHint,
	)
}

// normalizeVersion ensures the string starts with "v".
func normalizeVersion(v string) string {
	if v == "" {
		return v
	}
	if v[0] == 'v' {
		return v
	}
	return "v" + v
}

// Newer reports whether latest is a strictly newer semver than
// current. Both inputs may carry a leading "v"; non-numeric
// suffixes (pre-releases) cause Newer to return false to avoid
// spurious notices.
func Newer(latest, current string) bool {
	lMaj, lMin, lPatch, ok := parseSemver(latest)
	if !ok {
		return false
	}
	cMaj, cMin, cPatch, ok := parseSemver(current)
	if !ok {
		return false
	}
	if lMaj != cMaj {
		return lMaj > cMaj
	}
	if lMin != cMin {
		return lMin > cMin
	}
	return lPatch > cPatch
}

// parseSemver extracts (major, minor, patch) from a vX.Y.Z string.
// Returns ok=false on any parsing failure (including pre-release
// suffixes like "v0.2.0-rc1") so the caller errs on the side of
// silence.
func parseSemver(v string) (major, minor, patch int, ok bool) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	pat, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return maj, min, pat, true
}
