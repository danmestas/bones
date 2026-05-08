package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/timefmt"
)

// TestTimefmtSurface_StatusAsOfHeader pins the visible shape of
// `bones status` "as of" header for #324: HH:MM:SS followed by a
// zone abbreviation, with the local zone forced to a known value so
// the test passes on hosts in any zone.
func TestTimefmtSurface_StatusAsOfHeader(t *testing.T) {
	restore := withLocalZone(t, "America/Los_Angeles")
	defer restore()

	// Pick a January instant so the rendered abbreviation is PST,
	// not PDT — a stable target for the regex.
	rep := statusReport{
		WorkspaceDir:     "/tmp/ws/bones",
		GeneratedAt:      time.Date(2026, 1, 15, 20, 30, 45, 0, time.UTC),
		TasksByStatus:    map[tasks.Status]int{},
		TasksByID:        map[string]tasks.Task{},
		ScaffoldComplete: true,
	}
	var buf bytes.Buffer
	if err := renderStatus(rep, &buf); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}

	// 20:30:45 UTC → 12:30:45 PST.
	want := "as of 12:30:45 PST"
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("status header missing %q\n--- output ---\n%s",
			want, buf.String())
	}
}

// TestTimefmtSurface_TasksWatchBracket pins the visible shape of
// `bones tasks watch` bracket prefix for #324: [HH:MM:SS ZONE].
func TestTimefmtSurface_TasksWatchBracket(t *testing.T) {
	restore := withLocalZone(t, "America/Los_Angeles")
	defer restore()

	env := tasks.EventEnvelope{
		TaskID:    "abc",
		StreamSeq: 1,
		Timestamp: time.Date(2026, 1, 15, 20, 30, 45, 0, time.UTC),
		Type:      tasks.EventTypeCreated,
	}

	// printEventEnvelope writes to stdout directly. Use the
	// existing captureStdout helper from status_test.go.
	buf, finish := captureStdout(t)
	printEventEnvelope(env)
	finish()
	out := buf.String()

	want := "[12:30:45 PST]"
	if !strings.Contains(out, want) {
		t.Fatalf("tasks watch bracket missing %q\n--- output ---\n%s",
			want, out)
	}
}

// TestTimefmtSurface_UpLogIsUTC pins the load-bearing change for
// #324 on the up.log surface: every log line starts with a UTC
// RFC3339 timestamp (regex: digit-digit, "T", digit-digit, ..., "Z").
// Pre-#324 the timestamps were local with no zone marker.
func TestTimefmtSurface_UpLogIsUTC(t *testing.T) {
	dir := t.TempDir()

	l := openUpLog(dir)
	l.Infof("test-line")
	l.Close(nil)

	body, err := os.ReadFile(filepath.Join(dir, ".bones", "up.log"))
	if err != nil {
		t.Fatalf("read up.log: %v", err)
	}

	// First non-empty line: leading timestamp must match RFC3339 in
	// UTC (trailing "Z").
	re := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if !re.MatchString(line) {
			t.Errorf("up.log line not UTC RFC3339: %q\n--- full body ---\n%s",
				line, body)
		}
	}
}

// TestTimefmtSurface_CrossSurfaceConsistency exercises the
// cross-surface integration angle from the brief: the same instant
// observed via tasks_format.go formatTime (UTC RFC3339, route to
// `tasks show`) and via printEventEnvelope (local + zone, route to
// `tasks watch`) must trace back to identical seconds.
func TestTimefmtSurface_CrossSurfaceConsistency(t *testing.T) {
	restore := withLocalZone(t, "America/Los_Angeles")
	defer restore()

	instant := time.Date(2026, 1, 15, 20, 30, 45, 0, time.UTC)

	loggedShape := formatTime(instant)       // UTC RFC3339
	displayShape := timefmt.Display(instant) // local + zone

	// Parse the Logged shape back; must equal the original instant.
	parsed, err := time.Parse(time.RFC3339, loggedShape)
	if err != nil {
		t.Fatalf("Logged shape %q failed to parse: %v", loggedShape, err)
	}
	if !parsed.Equal(instant) {
		t.Errorf("Logged round-trip mismatch: %v != %v", parsed, instant)
	}

	// The Display shape extracts to 12:30:45 PST in this zone — the
	// same instant the Logged shape carries.
	if !strings.HasPrefix(displayShape, "12:30:45") {
		t.Errorf("Display shape %q lost wall-clock content for "+
			"the LA-zone view of the test instant", displayShape)
	}
	if !strings.HasSuffix(displayShape, " PST") {
		t.Errorf("Display shape %q missing PST zone marker", displayShape)
	}
}

// withLocalZone temporarily forces time.Local to the named zone for
// the duration of the test. Returns a restore function.
//
// Test isolation: every caller defers the restore so a zone-flapping
// test cannot leak into a subsequent one. Tests that don't call this
// run in whatever zone the host happens to be in — fine for tests
// that don't assert on the zone string.
func withLocalZone(t *testing.T, zone string) func() {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("load zone %q: %v", zone, err)
	}
	prev := time.Local
	time.Local = loc
	return func() { time.Local = prev }
}
