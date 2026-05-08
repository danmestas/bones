package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/cli/schemas"
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
		Timestamp: timefmt.NewLoggedTime(
			time.Date(2026, 1, 15, 20, 30, 45, 0, time.UTC)),
		Type: tasks.EventTypeCreated,
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

// TestTimefmtSurface_JSONPayloadEnvelopeIsUTC pins the load-bearing
// guarantee for B1: a representative payload struct with timestamp
// fields marshals every timestamp as UTC RFC3339 (Z-suffixed, no
// fractional seconds) regardless of system zone. Default time.Time
// marshaling would emit a local-zone offset string with nanoseconds
// — that's the regression LoggedTime exists to prevent.
func TestTimefmtSurface_JSONPayloadEnvelopeIsUTC(t *testing.T) {
	restore := withLocalZone(t, "America/Los_Angeles")
	defer restore()

	// The instant carries non-zero nanoseconds intentionally —
	// MarshalJSON must drop them.
	instant := time.Date(2026, 1, 15, 20, 30, 45, 999999999, time.UTC)
	payload := schemas.Task{
		ID:        "abc",
		Title:     "test",
		Status:    "open",
		Files:     []string{},
		CreatedAt: timefmt.NewLoggedTime(instant),
		UpdatedAt: timefmt.NewLoggedTime(instant),
	}

	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	got := string(b)

	// Both timestamp fields must end in Z (UTC), not -08:00 or
	// -07:00. A regression to the default time.Time marshal path
	// would produce the offset.
	wantSubstr := `"created_at":"2026-01-15T20:30:45Z"`
	if !strings.Contains(got, wantSubstr) {
		t.Errorf("payload missing Z-suffixed UTC created_at:\n%s",
			got)
	}
	wantUpdated := `"updated_at":"2026-01-15T20:30:45Z"`
	if !strings.Contains(got, wantUpdated) {
		t.Errorf("payload missing Z-suffixed UTC updated_at:\n%s",
			got)
	}

	// No nanoseconds anywhere.
	if strings.Contains(got, ".999") || strings.Contains(got, "999Z") {
		t.Errorf("payload kept fractional seconds:\n%s", got)
	}
	// No local-zone offset markers.
	if strings.Contains(got, "-08:00") || strings.Contains(got, "-07:00") {
		t.Errorf("payload leaked local-zone offset:\n%s", got)
	}
}

// TestTimefmtSurface_EventEnvelopeJSONIsUTC pins the same guarantee
// for tasks.EventEnvelope, the JetStream wire shape consumed by
// every recovery loop and watch consumer across bones versions.
func TestTimefmtSurface_EventEnvelopeJSONIsUTC(t *testing.T) {
	restore := withLocalZone(t, "Asia/Tokyo")
	defer restore()

	instant := time.Date(2026, 1, 15, 20, 30, 45, 0, time.UTC)
	env := tasks.EventEnvelope{
		Type:      tasks.EventTypeCreated,
		TaskID:    "abc",
		Timestamp: timefmt.NewLoggedTime(instant),
		Payload:   json.RawMessage(`{}`),
	}

	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	got := string(b)
	want := `"timestamp":"2026-01-15T20:30:45Z"`
	if !strings.Contains(got, want) {
		t.Errorf("envelope timestamp not UTC RFC3339:\n%s", got)
	}
	if strings.Contains(got, "+09:00") {
		t.Errorf("envelope leaked Tokyo zone offset:\n%s", got)
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
