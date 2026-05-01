// Package logwriter provides atomic NDJSON event writing with optional
// size-based rotation for workspace logs and no rotation for per-slot logs.
//
// Atomicity: each Append opens the file with O_APPEND|O_CREATE|O_WRONLY and
// writes a single newline-terminated line. POSIX guarantees that writes no
// larger than PIPE_BUF (512 bytes minimum; 4096+ on Linux and macOS) are
// atomic on append-only file descriptors, so concurrent slot processes cannot
// interleave lines for the small payloads we emit (~150–300 bytes).
//
// Rotation (workspace log only): on every Append, the file is stat-ed; if its
// size exceeds maxSize the numbered series is shifted (.log.1→.log.2, etc.),
// the current .log is renamed to .log.1, and a fresh .log is opened. The
// oldest numbered file is removed when the count exceeds maxFiles.
//
// Single-writer assumed for v1 (one process appending per file at a time).
// Concurrent-process safety relies on O_APPEND atomicity but NOT on rotation
// being race-free — do not call Append concurrently on the same Writer.
package logwriter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const (
	// defaultMaxSize is 10 MiB.
	defaultMaxSize = 10 * 1024 * 1024

	// defaultMaxFiles is the number of rotated backups kept (not counting the
	// active .log file).
	defaultMaxFiles = 5
)

// Writer writes NDJSON event lines to a single file, optionally rotating on
// size. Zero value is not valid; use Open or OpenSlot.
type Writer struct {
	path     string
	maxSize  int64 // 0 = no rotation
	maxFiles int
}

// Open returns a Writer for the workspace log at path.
// Defaults: 10 MiB maxSize, 5 backup files.
// Overridable via BONES_LOG_MAX_SIZE (bytes) and BONES_LOG_MAX_FILES.
func Open(path string) *Writer {
	maxSize := int64(defaultMaxSize)
	maxFiles := defaultMaxFiles

	if v := os.Getenv("BONES_LOG_MAX_SIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			maxSize = n
		}
	}
	if v := os.Getenv("BONES_LOG_MAX_FILES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxFiles = n
		}
	}

	return &Writer{path: path, maxSize: maxSize, maxFiles: maxFiles}
}

// OpenSlot returns a Writer for a per-slot log at <slotDir>/log.
// Rotation is disabled (maxSize=0) because per-slot logs are bounded by slot
// lifetime and must not rotate under the slot process's feet.
func OpenSlot(slotDir, slot string) *Writer {
	_ = slot // reserved for future use (e.g. naming)
	return &Writer{
		path:    filepath.Join(slotDir, "log"),
		maxSize: 0,
	}
}

// Append writes one event line atomically to the log file.
// If maxSize > 0 and the file has grown beyond it, rotation is performed first.
// The parent directory is created if absent.
func (w *Writer) Append(e Event) error {
	if w.maxSize > 0 {
		if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
			return fmt.Errorf("logwriter: mkdir %s: %w", filepath.Dir(w.path), err)
		}
		if err := w.rotateIfNeeded(); err != nil {
			return err
		}
	}
	return AppendOnce(w.path, e)
}

// AppendOnce writes one event line to path without rotation. Use this for
// per-slot logs and other call sites that don't carry rotation state across
// calls — it skips the Writer allocation entirely. The parent directory is
// created if absent.
//
// Atomicity: opens with O_APPEND|O_CREATE|O_WRONLY and writes a single
// newline-terminated line. POSIX guarantees writes ≤PIPE_BUF are atomic.
func AppendOnce(path string, e Event) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("logwriter: mkdir %s: %w", filepath.Dir(path), err)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("logwriter: marshal event: %w", err)
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logwriter: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("logwriter: write %s: %w", path, err)
	}
	return nil
}

// SlotLogPath returns the per-slot log path under <slotDir>/log. Per-slot logs
// are bounded by slot lifetime and never rotate, so callers should use
// AppendOnce(SlotLogPath(slotDir, slot), event) — no Writer needed.
func SlotLogPath(slotDir, slot string) string {
	_ = slot // reserved for future use (e.g. naming variants)
	return filepath.Join(slotDir, "log")
}

// rotateIfNeeded renames the current log file into the numbered backup series
// when its size exceeds maxSize. Older backups beyond maxFiles are removed.
//
// Shift order (n = maxFiles):
//
//	.log.n   → removed
//	.log.n-1 → .log.n
//	  ...
//	.log.1   → .log.2
//	.log     → .log.1
func (w *Writer) rotateIfNeeded() error {
	info, err := os.Stat(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to rotate
		}
		return fmt.Errorf("logwriter: stat %s: %w", w.path, err)
	}
	if info.Size() <= w.maxSize {
		return nil
	}

	// Drop the oldest backup if we're at capacity.
	oldest := w.path + "." + strconv.Itoa(w.maxFiles)
	_ = os.Remove(oldest) // best-effort; ignore ENOENT

	// Shift existing numbered backups up by one.
	for i := w.maxFiles - 1; i >= 1; i-- {
		src := w.path + "." + strconv.Itoa(i)
		dst := w.path + "." + strconv.Itoa(i+1)
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("logwriter: rotate %s → %s: %w", src, dst, err)
		}
	}

	// Rename active log to .log.1.
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("logwriter: rotate %s → %s.1: %w", w.path, w.path, err)
	}
	return nil
}
