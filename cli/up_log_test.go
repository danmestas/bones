package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenUpLog_CreatesFileAndBanner pins #171's load-bearing
// guarantee: opening the logger creates <wsDir>/.bones/up.log with a
// banner line so a closed terminal still leaves an audit trail.
func TestOpenUpLog_CreatesFileAndBanner(t *testing.T) {
	dir := t.TempDir()
	l := openUpLog(dir)
	l.Close(nil)

	logPath := filepath.Join(dir, ".bones", "up.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("up.log not created: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "starting") {
		t.Errorf("missing start banner in:\n%s", body)
	}
	if !strings.Contains(body, "finished") {
		t.Errorf("missing finished banner in:\n%s", body)
	}
	if !strings.Contains(body, "exit=0") {
		t.Errorf("missing exit code in:\n%s", body)
	}
}

// TestOpenUpLog_AppendsAcrossRuns pins #171's "idempotent on re-run"
// requirement: a second openUpLog must append to (not truncate) the
// existing file. Operators auditing a chain of `bones up` invocations
// rely on the cumulative history.
func TestOpenUpLog_AppendsAcrossRuns(t *testing.T) {
	dir := t.TempDir()

	first := openUpLog(dir)
	first.Infof("first-run-marker")
	first.Close(nil)

	logPath := filepath.Join(dir, ".bones", "up.log")
	beforeSize := fileSize(t, logPath)

	second := openUpLog(dir)
	second.Infof("second-run-marker")
	second.Close(nil)

	afterSize := fileSize(t, logPath)
	if afterSize <= beforeSize {
		t.Errorf("up.log shrank or stayed the same on re-run "+
			"(before=%d after=%d) — append mode broken", beforeSize, afterSize)
	}

	body, _ := os.ReadFile(logPath)
	for _, want := range []string{"first-run-marker", "second-run-marker"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("re-run lost %q from log:\n%s", want, body)
		}
	}
}

// TestOpenUpLog_RecordsExitCodeOnError pins the exit-code half of
// #171's defer contract: a failing run lands an "exit=1" line so a
// downstream debugger can grep for failure signatures.
func TestOpenUpLog_RecordsExitCodeOnError(t *testing.T) {
	dir := t.TempDir()
	l := openUpLog(dir)
	l.Close(errors.New("simulated step failure"))

	body, _ := os.ReadFile(filepath.Join(dir, ".bones", "up.log"))
	s := string(body)
	if !strings.Contains(s, "exit=1") {
		t.Errorf("expected exit=1 marker in:\n%s", s)
	}
	if !strings.Contains(s, "simulated step failure") {
		t.Errorf("expected error message in:\n%s", s)
	}
}

// TestUpLogger_NilSafe pins the soft-fail design: methods on a nil
// *upLogger fall back to plain stdout/stderr so a code path that
// can't open a workspace logger (early errors, tests) still functions.
func TestUpLogger_NilSafe(t *testing.T) {
	var l *upLogger
	l.Infof("info") // must not panic
	l.Warnf("warn") // must not panic
	l.Close(nil)    // must not panic

	w := l.Tee(io.Discard)
	if _, err := w.Write([]byte("ok\n")); err != nil {
		t.Errorf("nil-logger WriteTo failed: %v", err)
	}
}

// TestUpLogger_WriteToTees pins the printHubStatus integration: writes
// through the returned writer reach both the user-visible destination
// AND the log file so hub status lines are captured for audit.
func TestUpLogger_WriteToTees(t *testing.T) {
	dir := t.TempDir()
	l := openUpLog(dir)

	var visible bytes.Buffer
	w := l.Tee(&visible)
	if _, err := w.Write([]byte("up: hub: not yet started\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	l.Close(nil)

	if !strings.Contains(visible.String(), "not yet started") {
		t.Errorf("user-visible side missing payload: %q", visible.String())
	}
	body, _ := os.ReadFile(filepath.Join(dir, ".bones", "up.log"))
	if !strings.Contains(string(body), "not yet started") {
		t.Errorf("log file missing tee'd payload:\n%s", body)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Size()
}
